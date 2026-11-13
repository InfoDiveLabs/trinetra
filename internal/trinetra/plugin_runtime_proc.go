package trinetra

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
	"github.com/InfoDiveLabs/trinetra/internal/pluginrt"
	"github.com/InfoDiveLabs/trinetra/internal/update"
)

const (
	pluginUnitPollInterval = 2 * time.Second
	pluginSystemUser       = "trinetra-plugin"
)

// pluginSupervisor implements pluginrt.Supervisor with one companion per plugin.
type pluginSupervisor struct {
	d        *pluginDeps
	onFailed func(id, reason string)
	mu       sync.Mutex
	runs     map[string]*pluginRun
}

type pluginRun struct {
	stopCh   chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	mu       sync.Mutex
	restarts int
	up       bool
}

func (r *pluginRun) setUp(v bool) {
	r.mu.Lock()
	r.up = v
	r.mu.Unlock()
}

func (r *pluginRun) stop() {
	r.stopOnce.Do(func() { close(r.stopCh) })
	<-r.done
}

func (r *pluginRun) alive() bool {
	select {
	case <-r.done:
		return false
	default:
		return true
	}
}

var _ pluginrt.Supervisor = (*pluginSupervisor)(nil)

func newPluginSupervisor(d *pluginDeps, onFailed func(id, reason string)) *pluginSupervisor {
	return &pluginSupervisor{d: d, onFailed: onFailed, runs: map[string]*pluginRun{}}
}

func (s *pluginSupervisor) logf(format string, args ...any) {
	if s.d != nil && s.d.logf != nil {
		s.d.logf(format, args...)
	}
}

func (s *pluginSupervisor) clock() func() time.Time {
	if s.d != nil && s.d.now != nil {
		return s.d.now
	}
	return time.Now
}

// Start runs the plugin under a restarting companion, replacing any previous run.
func (s *pluginSupervisor) Start(rec plugin.Installed, binPath, token string) error {
	s.mu.Lock()
	old := s.runs[rec.ID]
	delete(s.runs, rec.ID)
	s.mu.Unlock()
	if old != nil {
		old.stop()
	}
	run := &pluginRun{stopCh: make(chan struct{}), done: make(chan struct{})}
	breaker := pluginrt.NewBreaker(s.clock())
	id := rec.ID
	crashed := func(cause error) bool {
		run.mu.Lock()
		run.restarts++
		run.up = false
		run.mu.Unlock()
		if !breaker.Crash() {
			return true
		}
		reason := fmt.Sprintf("crash loop: %d crashes in %d minutes", pluginrt.BreakerMaxCrashes, int(pluginrt.BreakerWindow/time.Minute))
		if cause != nil {
			reason += "; last error: " + cause.Error()
		}
		s.logf("plugin %s: %s", id, reason)
		if s.d != nil && s.d.deliverAlert != nil {
			s.d.deliverAlert(Alert{Key: "plugin." + id + ".crashloop", Severity: SevInfo, Kind: "fire", Source: "plugin",
				Title: fmt.Sprintf("Plugin %s stopped after repeated crashes; re-enable it to retry", rec.Manifest.Name),
				Body:  reason,
				Time:  s.clock()().Unix()})
		}
		if s.onFailed != nil {
			go s.onFailed(id, reason)
		}
		return false
	}
	spec := companionSpec{
		Name:    "plugin-" + id,
		Resolve: func() (string, error) { return verifyPluginBinary(rec, binPath) },
		Start: func(path string, _ []string) (supervisedProc, error) {
			p, err := pluginStartProc(rec, path, token)
			if err == nil {
				run.setUp(true)
			}
			return p, err
		},
		OnExit:      func(exitErr error, _ time.Duration) bool { return crashed(exitErr) },
		OnStartFail: crashed,
		Logf:        s.logf,
	}
	s.mu.Lock()
	s.runs[id] = run
	s.mu.Unlock()
	go superviseCompanion(spec, run.stopCh, run.done)
	return nil
}

// Stop stops the plugin and waits for its supervisor to exit.
func (s *pluginSupervisor) Stop(id string) error {
	s.mu.Lock()
	run := s.runs[id]
	delete(s.runs, id)
	s.mu.Unlock()
	if run != nil {
		run.stop()
	}
	return nil
}

// Running reports whether the plugin's supervisor is active and how many times it exited.
func (s *pluginSupervisor) Running(id string) (bool, int) {
	s.mu.Lock()
	run := s.runs[id]
	s.mu.Unlock()
	if run == nil {
		return false, 0
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	return run.up && run.alive(), run.restarts
}

// verifyPluginBinary re-checks binPath against the manifest hash and ownership before every spawn.
func verifyPluginBinary(rec plugin.Installed, binPath string) (string, error) {
	b, ok := rec.Manifest.BinaryFor(runtime.GOOS, runtime.GOARCH)
	if !ok {
		return "", fmt.Errorf("manifest has no binary for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	info, err := os.Lstat(binPath)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", binPath)
	}
	if err := verifyOwnerAndPerms(binPath, info, os.Geteuid()); err != nil {
		return "", err
	}
	sum, err := sha256File(binPath)
	if err != nil {
		return "", err
	}
	if sum != b.SHA256 {
		return "", fmt.Errorf("%s checksum mismatch (expected %s, got %s)", binPath, b.SHA256, sum)
	}
	return binPath, nil
}

// unitProc adapts a systemd unit to supervisedProc: Start = systemctl start, Wait = poll `systemctl show -p ActiveState,Result` every 2s until inactive/failed, Kill = systemctl stop.
type unitProc struct {
	unit  string
	x     Exec
	sleep func(time.Duration)
}

func pluginUnitName(id string) string { return "trinetra-plugin@" + id + ".service" }

func startUnitProc(x Exec, id string) (*unitProc, error) {
	u := pluginUnitName(id)
	_, _ = x.Run("systemctl", "reset-failed", u)
	if out, err := x.Run("systemctl", "start", u); err != nil {
		return nil, fmt.Errorf("systemctl start %s: %v: %s", u, err, strings.TrimSpace(string(out)))
	}
	return &unitProc{unit: u, x: x, sleep: time.Sleep}, nil
}

func (p *unitProc) Wait() error {
	for {
		p.sleep(pluginUnitPollInterval)
		out, err := p.x.Run("systemctl", "show", "-p", "ActiveState,Result", p.unit)
		if err != nil {
			return fmt.Errorf("systemctl show %s: %w", p.unit, err)
		}
		state, result := "", ""
		for _, line := range strings.Split(string(out), "\n") {
			k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
			switch k {
			case "ActiveState":
				state = v
			case "Result":
				result = v
			}
		}
		switch state {
		case "failed":
			return fmt.Errorf("unit %s failed (%s)", p.unit, result)
		case "inactive":
			if result == "" || result == "success" {
				return nil
			}
			return fmt.Errorf("unit %s exited (%s)", p.unit, result)
		}
	}
}

func (p *unitProc) Kill() error {
	_, err := p.x.Run("systemctl", "stop", p.unit)
	return err
}

// hasSystemd is the seam over /run/systemd/system existence.
var hasSystemd = func() bool {
	st, err := os.Stat("/run/systemd/system")
	return err == nil && st.IsDir()
}

// pluginExecutor runs the systemctl commands of the unit path.
var pluginExecutor Exec = osExec{}

// pluginStartProc is the seam: systemd present -> unit (writes drop-in, daemon-reload once per change); else exec `<self> plugin-exec <id>`; dev or non-root -> direct exec.
var pluginStartProc = defaultPluginStartProc

func defaultPluginStartProc(rec plugin.Installed, binPath, token string) (supervisedProc, error) {
	d := currentPluginDeps
	if d == nil {
		return nil, fmt.Errorf("plugin runtime is not wired")
	}
	tokenPath := filepath.Join(d.stateDir, "plugins", rec.ID, plugin.TokenFileName)
	hostSock := filepath.Join(d.hostDir, "host.sock")
	if pluginIsRoot() && hasSystemd() {
		if err := writePluginUnitFn(pluginExecutor, rec, binPath, hostSock, tokenPath); err != nil {
			return nil, err
		}
		return startUnitProc(pluginExecutor, rec.ID)
	}
	return startExecProc(rec, binPath, hostSock, tokenPath)
}

// currentPluginDeps is set by wireRuntime so the default process starter can find paths.
var currentPluginDeps *pluginDeps

var (
	unitWriteMu       sync.Mutex
	writePluginUnitFn = writePluginUnit
	pluginIsRoot      = func() bool { return os.Geteuid() == 0 }
)

func writePluginUnit(x Exec, rec plugin.Installed, binPath, hostSock, tokenPath string) error {
	unitWriteMu.Lock()
	defer unitWriteMu.Unlock()
	reload := false
	if _, err := os.Stat(pluginrt.TemplateUnitPath); err != nil {
		if err := update.WriteFileAtomic(pluginrt.TemplateUnitPath, []byte(pluginrt.RenderTemplateUnit()), 0o644); err != nil {
			return err
		}
		reload = true
	}
	body := pluginrt.RenderDropIn(rec.ID, binPath, hostSock, tokenPath, rec.Granted, pluginrt.DefaultLimits)
	if body == "" {
		return fmt.Errorf("plugin id %q is invalid", rec.ID)
	}
	path := pluginrt.DropInPath(rec.ID)
	if cur, err := os.ReadFile(path); err != nil || string(cur) != body {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := update.WriteFileAtomic(path, []byte(body), 0o644); err != nil {
			return err
		}
		reload = true
	}
	if reload {
		if out, err := x.Run("systemctl", "daemon-reload"); err != nil {
			return fmt.Errorf("systemctl daemon-reload: %v: %s", err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

var (
	pluginPrepareRuntimeDir = prepareRuntimeDir
	pluginEnsureUser        = ensurePluginUser
)

func prepareRuntimeDir(id string, chownToPluginUser bool) error {
	dir := filepath.Dir(pluginrt.PluginSocketPath(id))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(pluginrt.SocketRoot, 0o711); err != nil {
		return err
	}
	if chownToPluginUser {
		u, err := user.Lookup(pluginSystemUser)
		if err != nil {
			return err
		}
		uid, _ := strconv.Atoi(u.Uid)
		gid, _ := strconv.Atoi(u.Gid)
		if err := os.Chown(dir, uid, gid); err != nil {
			return err
		}
	}
	return nil
}

func startExecProc(rec plugin.Installed, binPath, hostSock, tokenPath string) (supervisedProc, error) {
	root := pluginIsRoot()
	if err := pluginPrepareRuntimeDir(rec.ID, root); err != nil {
		return nil, err
	}
	var cmd *exec.Cmd
	if root {
		self, err := os.Executable()
		if err != nil {
			return nil, err
		}
		cmd = exec.Command(self, "plugin-exec", rec.ID, binPath, tokenPath)
		cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", plugin.EnvHostSocket + "=" + hostSock}
	} else {
		cmd = exec.Command(binPath)
		cmd.Env = pluginEnv(rec.ID, hostSock, tokenPath, filepath.Join(filepath.Dir(tokenPath), "data"))
	}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &execProc{cmd: cmd}, nil
}

func pluginEnv(id, hostSock, tokenPath, dataDir string) []string {
	return []string{
		"PATH=/usr/sbin:/usr/bin:/sbin:/bin",
		plugin.EnvPluginID + "=" + id,
		plugin.EnvHostSocket + "=" + hostSock,
		plugin.EnvPluginSocket + "=" + pluginrt.PluginSocketPath(id),
		plugin.EnvTokenFile + "=" + tokenPath,
		plugin.EnvDataDir + "=" + dataDir,
	}
}
