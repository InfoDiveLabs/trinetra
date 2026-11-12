package trinetra

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

// pluginStdin is the seam for interactive prompts.
var pluginStdin io.Reader = os.Stdin

const maxStatusLogs = 10

type pluginPrompt struct {
	in *bufio.Reader
}

func (p *pluginPrompt) line(prompt string) (string, error) {
	fmt.Fprint(stdout, prompt)
	s, err := p.in.ReadString('\n')
	if err != nil && (err != io.EOF || s == "") {
		return "", err
	}
	return strings.TrimSpace(s), nil
}

func (p *pluginPrompt) yes(prompt string) bool {
	s, err := p.line(prompt)
	if err != nil {
		return false
	}
	s = strings.ToLower(s)
	return s == "y" || s == "yes"
}

func pluginActor() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "cli"
}

func permNames(ps []plugin.Permission) string {
	s := make([]string, len(ps))
	for i, p := range ps {
		s[i] = string(p)
	}
	return strings.Join(s, ", ")
}

// cmdPluginRuntime handles install|enable|disable|remove|update|set|list|status|dev.
func cmdPluginRuntime(sub string, args []string) int {
	var err error
	switch sub {
	case "install":
		err = pluginInstall(args)
	case "enable":
		err = pluginEnable(args)
	case "disable", "remove":
		err = pluginDisableRemove(sub, args)
	case "update":
		err = pluginUpdate(args)
	case "set":
		err = pluginSet(args)
	case "list":
		err = pluginList()
	case "status":
		err = pluginStatus(args)
	case "dev":
		err = pluginDev(args)
	default:
		fmt.Fprintln(stderr, pluginUsage)
		return 2
	}
	var ue usageError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ue):
		fmt.Fprintf(stderr, "trinetra plugin %s: %s\nusage: %s\n", sub, ue.msg, ue.usage)
		return 2
	default:
		fmt.Fprintf(stderr, "trinetra plugin %s: %v\n", sub, err)
		return 1
	}
}

type usageError struct{ msg, usage string }

func (e usageError) Error() string { return e.msg }

func splitFlags(args []string) (pos []string, flags map[string]string) {
	flags = map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			pos = append(pos, a)
			continue
		}
		k, v, hasV := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		if !hasV && (k == "sha256" || k == "bin") && i+1 < len(args) {
			i++
			v = args[i]
		}
		flags[k] = v
	}
	return pos, flags
}

func pluginInstall(args []string) error {
	pos, flags := splitFlags(args)
	if len(pos) != 1 {
		return usageError{"want exactly one reference", "trinetra plugin install <id[@version] | file | url> [--sha256 HEX]"}
	}
	ref := pos[0]
	isURL := strings.HasPrefix(ref, "https://") || strings.HasPrefix(ref, "http://")
	if !isURL && (strings.ContainsAny(ref, "/\\") || strings.HasSuffix(ref, ".tar.gz") || strings.HasSuffix(ref, ".tgz")) {
		if abs, err := filepath.Abs(ref); err == nil {
			ref = abs
		}
	}
	var res plugin.InstallResult
	err := pluginWithClient(func(m plugin.Manager) error {
		var err error
		res, err = m.Install(plugin.InstallRequest{Ref: ref, Actor: pluginActor(), Via: "cli", ExpectSHA256: flags["sha256"]})
		return err
	})
	if err != nil {
		return err
	}
	verb := "installed"
	if res.Updated {
		verb = "updated"
	} else if len(res.NewPermissions) > 0 {
		verb = "downloaded (update pending consent)"
	}
	fmt.Fprintf(stdout, "%s %s %s (%s)\n", verb, res.ID, res.Version, res.Trust)
	if len(res.Permissions) == 0 {
		fmt.Fprintln(stdout, "permissions: none")
	} else {
		fmt.Fprintln(stdout, "permissions:")
		for _, p := range res.Permissions {
			fmt.Fprintf(stdout, "  - %s: %s\n", p, plugin.Describe(p))
		}
	}
	if res.Trust != plugin.TrustVerified {
		fmt.Fprintln(stdout, "note: this plugin is UNVERIFIED; enabling it asks for explicit risk acknowledgement.")
	}
	fmt.Fprintf(stdout, "next: trinetra plugin enable %s\n", res.ID)
	return nil
}

func pluginEnable(args []string) error {
	pos, flags := splitFlags(args)
	if len(pos) != 1 {
		return usageError{"want a plugin id", "trinetra plugin enable <id> [--accept=perm1,perm2] [--i-understand-the-risk]"}
	}
	id := pos[0]
	in := &pluginPrompt{in: bufio.NewReader(pluginStdin)}
	return pluginWithClient(func(m plugin.Manager) error {
		st, err := m.Get(id)
		if err != nil {
			return err
		}
		perms, ver, trust, pending := st.Manifest.Permissions, st.Version, st.Trust, false
		name, publisher := st.Manifest.Name, st.Manifest.Publisher
		if st.Pending != nil {
			perms, ver, trust, pending = st.Pending.Manifest.Permissions, st.Pending.Version, st.Pending.Trust, true
			name, publisher = st.Pending.Manifest.Name, st.Pending.Manifest.Publisher
		}
		unverified := trust != plugin.TrustVerified && trust != plugin.TrustFirstParty
		req := plugin.EnableRequest{ID: id, Actor: pluginActor(), Via: "cli", Pending: pending}
		if acc, ok := flags["accept"]; ok {
			if err := checkAccept(acc, perms); err != nil {
				return err
			}
			if unverified {
				if _, ok := flags["i-understand-the-risk"]; !ok {
					return errors.New("unverified plugin: pass --i-understand-the-risk together with --accept")
				}
				req.AcknowledgedRisk = true
			}
			req.Accepted = perms
		} else if unverified {
			fmt.Fprintln(stdout, plugin.RiskWarning(name, ver, publisher))
			if s, err := in.line(`Type "yes" to continue: `); err != nil || s != "yes" {
				return errors.New("aborted; nothing granted")
			}
			req.AcknowledgedRisk = true
			for _, p := range perms {
				if !in.yes(fmt.Sprintf("Allow %s (%s)? [y/N] ", p, plugin.Describe(p))) {
					return errors.New("aborted; nothing granted")
				}
				req.Accepted = append(req.Accepted, p)
			}
		} else {
			fmt.Fprintf(stdout, "%s %s requests:\n", name, ver)
			for _, p := range perms {
				fmt.Fprintf(stdout, "  - %s: %s\n", p, plugin.Describe(p))
			}
			if !in.yes("Grant these permissions? [y/N] ") {
				return errors.New("aborted; nothing granted")
			}
			req.Accepted = perms
		}
		if err := m.Enable(req); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "enabled %s %s\n", id, ver)
		return nil
	})
}

func checkAccept(acc string, perms []plugin.Permission) error {
	var got []plugin.Permission
	for _, p := range strings.Split(acc, ",") {
		if p = strings.TrimSpace(p); p != "" {
			got = append(got, plugin.Permission(p))
		}
	}
	if len(plugin.Missing(got, perms)) > 0 || len(plugin.Missing(perms, got)) > 0 {
		sorted := append([]plugin.Permission(nil), perms...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		return fmt.Errorf("--accept must list exactly the requested permissions: %s", permNames(sorted))
	}
	return nil
}

func pluginDisableRemove(sub string, args []string) error {
	pos, _ := splitFlags(args)
	if len(pos) != 1 {
		return usageError{"want a plugin id", "trinetra plugin " + sub + " <id>"}
	}
	return pluginWithClient(func(m plugin.Manager) error {
		var err error
		if sub == "disable" {
			err = m.Disable(pos[0], pluginActor(), "cli")
		} else {
			err = m.Remove(pos[0], pluginActor(), "cli")
		}
		if err == nil {
			fmt.Fprintf(stdout, "%s %s\n", map[string]string{"disable": "disabled", "remove": "removed"}[sub], pos[0])
		}
		return err
	})
}

func pluginUpdate(args []string) error {
	pos, flags := splitFlags(args)
	_, all := flags["all"]
	if len(pos) > 1 || (len(pos) == 1 && all) {
		return usageError{"want a plugin id or --all", "trinetra plugin update [<id> | --all]"}
	}
	id := ""
	if len(pos) == 1 {
		id = pos[0]
	}
	var res []plugin.UpdateResult
	err := pluginWithClient(func(m plugin.Manager) error {
		var err error
		res, err = m.Update(plugin.UpdateRequest{ID: id, Actor: pluginActor(), Via: "cli"})
		return err
	})
	if err != nil {
		return err
	}
	if len(res) == 0 {
		fmt.Fprintln(stdout, "everything is up to date")
	}
	failed := false
	for _, r := range res {
		switch {
		case r.Error != "":
			failed = true
			fmt.Fprintf(stdout, "%s: %s\n", r.ID, r.Error)
		case len(r.NeedsConsent) > 0:
			fmt.Fprintf(stdout, "%s %s -> %s: run: trinetra plugin enable %s (new permissions: %s)\n", r.ID, r.From, r.To, r.ID, permNames(r.NeedsConsent))
		case r.From == r.To:
			fmt.Fprintf(stdout, "%s %s is up to date\n", r.ID, r.From)
		default:
			fmt.Fprintf(stdout, "%s %s -> %s\n", r.ID, r.From, r.To)
		}
	}
	if failed {
		return errors.New("some updates failed")
	}
	return nil
}

func pluginSet(args []string) error {
	pos, _ := splitFlags(args)
	if len(pos) < 2 {
		return usageError{"want an id and key=value pairs", "trinetra plugin set <id> key=value [key=value ...]  (key=- reads a secret from stdin)"}
	}
	id := pos[0]
	vals := map[string]string{}
	in := bufio.NewReader(pluginStdin)
	for _, kv := range pos[1:] {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return usageError{fmt.Sprintf("%q is not key=value", kv), "trinetra plugin set <id> key=value [key=value ...]"}
		}
		if v == "-" {
			s, err := readSecretLine(in)
			if err != nil {
				return err
			}
			v = s
		}
		vals[k] = v
	}
	if err := pluginWithClient(func(m plugin.Manager) error {
		return m.SetConfig(plugin.SetConfigRequest{ID: id, Values: vals, Actor: pluginActor(), Via: "cli"})
	}); err != nil {
		return err
	}
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintf(stdout, "updated %s: %s\n", id, strings.Join(keys, ", "))
	return nil
}

func readSecretLine(in *bufio.Reader) (string, error) {
	if f, ok := pluginStdin.(*os.File); ok {
		if st, err := f.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
			off := exec.Command("stty", "-echo")
			off.Stdin = f
			if off.Run() == nil {
				defer func() {
					on := exec.Command("stty", "echo")
					on.Stdin = f
					_ = on.Run()
					fmt.Fprintln(stdout)
				}()
			}
		}
	}
	s, err := in.ReadString('\n')
	if err != nil && (err != io.EOF || s == "") {
		return "", fmt.Errorf("reading secret from stdin: %w", err)
	}
	return strings.TrimRight(s, "\r\n"), nil
}

func pluginList() error {
	var l []plugin.Status
	if err := pluginWithClient(func(m plugin.Manager) error {
		var err error
		l, err = m.List()
		return err
	}); err != nil {
		return err
	}
	if len(l) == 0 {
		fmt.Fprintln(stdout, "no plugins installed")
		return nil
	}
	tw := tabwriter.NewWriter(stdout, 0, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tVERSION\tTRUST\tSTATE\tUPDATE")
	for _, s := range l {
		up := s.UpdateAvailable
		if up == "" {
			up = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", s.ID, s.Version, s.Trust, s.State, up)
	}
	return tw.Flush()
}

func pluginStatus(args []string) error {
	pos, _ := splitFlags(args)
	if len(pos) != 1 {
		return usageError{"want a plugin id", "trinetra plugin status <id>"}
	}
	var s plugin.Status
	if err := pluginWithClient(func(m plugin.Manager) error {
		var err error
		s, err = m.Get(pos[0])
		return err
	}); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s %s (%s)\n", s.Manifest.Name, s.Version, s.Trust)
	fmt.Fprintf(stdout, "state:    %s", s.State)
	if s.Reason != "" {
		fmt.Fprintf(stdout, " (%s)", s.Reason)
	}
	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "running:  %v (restarts %d)\n", s.Running, s.Restarts)
	h := s.Health
	status := h.Status.State
	if status == "" {
		status = "unknown"
	}
	fmt.Fprintf(stdout, "health:   %s", status)
	if h.Status.Message != "" {
		fmt.Fprintf(stdout, " - %s", h.Status.Message)
	}
	fmt.Fprintf(stdout, " (connected %v, %d series, %d rejected)\n", h.Connected, h.SeriesCount, h.SeriesRejected)
	if s.UpdateAvailable != "" {
		fmt.Fprintf(stdout, "update:   %s available\n", s.UpdateAvailable)
	}
	if s.RevokedReason != "" {
		fmt.Fprintf(stdout, "revoked:  %s\n", s.RevokedReason)
	}
	if len(s.Granted) > 0 {
		fmt.Fprintf(stdout, "granted:  %s\n", permNames(s.Granted))
	}
	if len(s.Consents) > 0 {
		fmt.Fprintln(stdout, "consents:")
		for _, c := range s.Consents {
			fmt.Fprintf(stdout, "  - %s via %s, version %s: %s\n", c.Actor, c.Via, c.Version, permNames(c.Permissions))
		}
	}
	logs := h.RecentLogs
	if len(logs) > maxStatusLogs {
		logs = logs[len(logs)-maxStatusLogs:]
	}
	if len(logs) > 0 {
		fmt.Fprintln(stdout, "recent logs:")
		for _, l := range logs {
			fmt.Fprintf(stdout, "  [%s] %s\n", l.Level, l.Message)
		}
	}
	return nil
}

func pluginDev(args []string) error {
	pos, flags := splitFlags(args)
	if len(pos) != 1 {
		return usageError{"want a plugin directory", "trinetra plugin dev <dir> [--bin PATH]"}
	}
	dir, err := filepath.Abs(pos[0])
	if err != nil {
		return err
	}
	return pluginWithClient(func(m plugin.Manager) error {
		sess, err := m.DevAttach(plugin.DevRequest{Dir: dir, Actor: pluginActor()})
		if err != nil {
			return err
		}
		defer func() { _ = m.DevDetach(sess.ID) }()
		fmt.Fprintf(stdout, "DEV MODE: %s is unsigned and NOT sandboxed. Do not use on production hosts.\n", sess.ID)
		bin := flags["bin"]
		if bin == "" {
			bin = filepath.Join(dir, filepath.FromSlash(plugin.BinaryPath(runtime.GOOS, runtime.GOARCH, sess.ID)))
		}
		tmp, err := os.MkdirTemp("", "trinetra-plugin-dev-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		tokenFile := filepath.Join(tmp, "token")
		if err := os.WriteFile(tokenFile, []byte(sess.Token), 0o600); err != nil {
			return err
		}
		dataDir := filepath.Join(tmp, "data")
		if err := os.Mkdir(dataDir, 0o700); err != nil {
			return err
		}
		cmd := exec.Command(bin)
		cmd.Env = append(os.Environ(),
			plugin.EnvPluginID+"="+sess.ID,
			plugin.EnvHostSocket+"="+sess.HostSocket,
			plugin.EnvPluginSocket+"="+sess.PluginSocket,
			plugin.EnvTokenFile+"="+tokenFile,
			plugin.EnvDataDir+"="+dataDir,
			plugin.EnvDev+"=1",
		)
		cmd.Stdout, cmd.Stderr = stdout, stderr
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(sigs)
		if err := cmd.Start(); err != nil {
			return err
		}
		exited := make(chan struct{})
		go func() {
			for {
				select {
				case s := <-sigs:
					_ = cmd.Process.Signal(s)
				case <-exited:
					return
				}
			}
		}()
		err = cmd.Wait()
		close(exited)
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			fmt.Fprintf(stdout, "plugin exited: %v\n", ee)
			return nil
		}
		return err
	})
}
