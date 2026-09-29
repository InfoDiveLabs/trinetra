// Package trinetra: update_cmd.go implements `trinetra update
// status|check|apply|rollback|guard`, the operator/systemd-facing surface
// over the self-update primitives built in internal/update and
// update_apply.go (fetch/verify, plan, stage, smoke-test, swap, restore).
package trinetra

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/update"
	"github.com/InfoDiveLabs/trinetra/internal/version"
)

// updater bundles everything a self-update operation needs, all of it
// injectable for tests. Production callers get one from newUpdater.
type updater struct {
	paths       updatePaths
	keys        update.KeySet
	x           Exec
	now         func() time.Time
	arch        string
	running     update.Version
	launchGuard func() error
	// src overrides the source an operation fetches from; nil means "build
	// it from config" (updateSource(c)) -- see Ruling R1 in
	// .superpowers/sdd/2026-09-29-signed-releases-self-update/progress.md.
	src update.Source
}

// applyOptions are `trinetra update apply`'s flags, or the equivalent when
// called from an updater method directly (e.g. tests).
type applyOptions struct {
	Version string
	Bundle  string
	Channel string
	Force   bool
}

// updateStatus is `trinetra update status`'s JSON/table shape.
type updateStatus struct {
	Running      string          `json:"running"`
	Channel      string          `json:"channel"`
	Source       string          `json:"source"`
	Floor        string          `json:"floor"`
	Available    string          `json:"available"`
	Previous     string          `json:"previous"`
	Pending      *update.Pending `json:"pending"`
	Last         *update.Result  `json:"last"`
	KeysLoaded   bool            `json:"keys_loaded"`
	Fingerprints []string        `json:"fingerprints"`
}

// clock returns u.now, or time.Now if it was left nil.
func (u updater) clock() time.Time {
	if u.now != nil {
		return u.now()
	}
	return time.Now()
}

// updateSource builds the Source `trinetra update` operations use when the
// updater's own src field is nil: github when update.source=github (the
// default), nil (meaning "no source configured") when update.source=none.
func updateSource(c *config.Config) update.Source {
	switch c.UpdateSource() {
	case "github":
		// BaseURL is "" (the real GitHub API) in every build except
		// trinetra_testkeys with TRINETRA_E2E_GITHUB_BASE_URL set -- see
		// update_e2e_hooks.go/update_e2e_hooks_testkeys.go. That is how the
		// test/docker/update e2e harness points a real `update.source=github`
		// host at its fake GitHub API (relsrv) without any config setting a
		// production host could accidentally point at an attacker's server.
		return update.GitHubSource{Repo: "InfoDiveLabs/trinetra", Token: c.Update.GitHubToken, BaseURL: e2eGitHubBaseURL()}
	default:
		return nil
	}
}

// resolveSource picks the source for one operation: an explicit --bundle
// directory first, then the updater's own src (set by tests), then one
// built fresh from config.
func (u updater) resolveSource(c *config.Config, bundle string) update.Source {
	if bundle != "" {
		return update.DirSource{Dir: bundle}
	}
	if u.src != nil {
		return u.src
	}
	return updateSource(c)
}

// check fetches and verifies the channel pointer and the release it names,
// checks it against host policy (floor/running), and records the outcome
// (LastCheck, LastPointerIssued, and Available when a newer release exists)
// in state. It returns the fetched manifest even when the policy check
// fails (e.g. ErrAlreadyInstalled), so a caller can still report what's on
// the channel.
func (u updater) check(ctx context.Context, c *config.Config) (update.Manifest, error) {
	channel := c.UpdateChannel()
	if channel == "off" {
		return update.Manifest{}, fmt.Errorf("updates are off (update.channel=off)")
	}
	src := u.resolveSource(c, "")
	if src == nil {
		return update.Manifest{}, fmt.Errorf("update.source is none; use --bundle DIR")
	}

	now := u.clock()
	ptr, err := update.FetchLatest(ctx, src, u.keys, channel, now)
	if err != nil {
		return update.Manifest{}, err
	}
	m, _, err := update.FetchRelease(ctx, src, u.keys, ptr.Version)
	if err != nil {
		return update.Manifest{}, err
	}

	st, err := update.LoadState(u.paths.dir())
	if err != nil {
		return update.Manifest{}, err
	}
	floor := st.FloorVersion(u.running)
	policyErr := update.CheckPolicy(m, update.Policy{Channel: channel, Floor: floor, Running: u.running})

	if policyErr == nil && st.IsBad(m.Version) {
		policyErr = fmt.Errorf("%w: %s", errKnownBad, m.Version)
	}

	st.LastCheck = now.Unix()
	st.LastPointerIssued = ptr.Issued
	// R18: Available drives the "update available" alert, the web Apply
	// button and Telegram /version, so it names only a release this host
	// would actually accept (channel, floor, min_upgrade_from, not known
	// bad); anything else clears it.
	st.Available = ""
	if policyErr == nil {
		st.Available = m.Version
	}
	if err := update.SaveState(u.paths.dir(), st); err != nil {
		return update.Manifest{}, err
	}
	return m, policyErr
}

// errKnownBad is check's verdict for a release that verifies and passes
// policy but already failed its health check on this host: it is not
// offered as available (apply --force can still install it).
var errKnownBad = errors.New("update: release failed its health check here before")

// preflightApply runs apply's fast, synchronous-only checks (fix round 1,
// Ruling R10): the channel/source settings apply itself would otherwise only
// refuse on after resolving them again, and persisted state being both
// readable and free of an already-pending update -- the same
// already-pending refusal swapIn (update_apply.go) would otherwise only
// discover AFTER a network fetch and local staging/smoke-test have already
// run. The control-socket-facing UpdateApply (coreapi_inproc.go) calls this
// synchronously before handing the rest of apply's work to a background
// goroutine, so a caller that has no hope of succeeding (updates off, no
// source configured, another update already pending) gets a fast, accurate
// error instead of a false "started". It deliberately does NOT duplicate
// apply's version-resolution/fetch/policy/plan/stage/smoke-test/swap steps --
// those still only run inside apply itself, synchronously or not.
func (u updater) preflightApply(c *config.Config, opts applyOptions) error {
	channel := opts.Channel
	if channel == "" {
		channel = c.UpdateChannel()
	}
	if channel == "off" && opts.Bundle == "" {
		return fmt.Errorf("updates are off (update.channel=off)")
	}
	if u.resolveSource(c, opts.Bundle) == nil {
		return fmt.Errorf("update.source is none; use --bundle DIR")
	}
	st, err := update.LoadState(u.paths.dir())
	if err != nil {
		return err
	}
	if st.Pending != nil {
		return errUpdateInProgress
	}
	return nil
}

// preflightRollback runs rollback's fast, synchronous-only checks (fix
// round 1, Ruling R10): the exact same "is there a previous build" and
// "is an update already pending" checks rollback itself runs first (see
// rollback's own doc below) -- duplicated here (rather than factored out)
// because rollback's version stays the single source of truth callers that
// invoke it directly (the CLI's `trinetra update rollback`) rely on; this
// copy exists purely so the control-socket-facing UpdateRollback
// (coreapi_inproc.go) can run it synchronously before backgrounding the rest
// (restorePrevious + launching the guard).
func (u updater) preflightRollback() error {
	prevCore := filepath.Join(u.paths.previous(), "trinetra")
	fi, err := os.Stat(prevCore)
	if err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("update: no previous build to roll back to")
	}
	st, err := update.LoadState(u.paths.dir())
	if err != nil {
		return err
	}
	if st.Pending != nil {
		return fmt.Errorf("update: an update is already pending (see trinetra update status)")
	}
	return nil
}

// apply installs a signed release: resolve channel/source/version, fetch
// and verify the release, check policy and the known-bad list, plan which
// binaries to install, stage and smoke-test them, swap them in, then start
// the health guard. Any failure once the guard has been asked to start (but
// failed to) restores the previous build and clears the pending marker, so
// a host is never left "pending" on a guard that never started.
func (u updater) apply(ctx context.Context, c *config.Config, opts applyOptions) (update.Manifest, error) {
	channel := opts.Channel
	if channel == "" {
		channel = c.UpdateChannel()
	}
	if channel == "off" && opts.Bundle == "" {
		return update.Manifest{}, fmt.Errorf("updates are off (update.channel=off)")
	}

	src := u.resolveSource(c, opts.Bundle)
	if src == nil {
		return update.Manifest{}, fmt.Errorf("update.source is none; use --bundle DIR")
	}

	// version resolution: an explicit --version wins; otherwise a network
	// source needs FetchLatest to learn which release to fetch, but a
	// bundle directory holds exactly one release and DirSource.ReleaseAsset
	// ignores the version argument entirely -- FetchRelease below reads the
	// real version straight out of the bundle's own (verified) manifest.
	reqVersion := opts.Version
	if reqVersion == "" && opts.Bundle == "" {
		ptr, err := update.FetchLatest(ctx, src, u.keys, channel, u.clock())
		if err != nil {
			return update.Manifest{}, err
		}
		reqVersion = ptr.Version
	}

	m, raw, err := update.FetchRelease(ctx, src, u.keys, reqVersion)
	if err != nil {
		return update.Manifest{}, err
	}

	// Ruling R7: update.channel=off only ever refuses the network source
	// (checked above, before src is even resolved); an explicit --bundle
	// install must still go through even with updates off. Once the bundle
	// manifest is signature-verified (FetchRelease, just above), gate it on
	// its own channel rather than refusing on "off" -- floor,
	// min_upgrade_from and the bad-version check below are unaffected.
	policyChannel := channel
	if channel == "off" && opts.Bundle != "" {
		policyChannel = m.Channel
	}

	st, err := update.LoadState(u.paths.dir())
	if err != nil {
		return update.Manifest{}, err
	}
	floor := st.FloorVersion(u.running)
	if err := update.CheckPolicy(m, update.Policy{Channel: policyChannel, Floor: floor, Running: u.running}); err != nil {
		return update.Manifest{}, err
	}
	if st.IsBad(m.Version) && !opts.Force {
		return update.Manifest{}, fmt.Errorf("update: %s failed its health check here before; use --force to try again", m.Version)
	}

	installed := func(name string) bool {
		fi, err := os.Stat(filepath.Join(u.paths.BinDir, name))
		return err == nil && fi.Mode().IsRegular()
	}
	plan, err := planApply(m, raw, u.arch, installed)
	if err != nil {
		return update.Manifest{}, err
	}

	if err := stage(ctx, u.paths, src, plan); err != nil {
		return update.Manifest{}, err
	}

	stagedCore := filepath.Join(u.paths.staging(m.Version), "trinetra-linux-"+u.arch)
	if err := smokeTest(u.x, stagedCore, m.Version); err != nil {
		return update.Manifest{}, err
	}

	if err := swapIn(u.paths, plan, u.clock()); err != nil {
		return update.Manifest{}, err
	}

	if err := u.launchGuard(); err != nil {
		restorePrevious(u.paths)
		if st2, lerr := update.LoadState(u.paths.dir()); lerr == nil {
			st2.Pending = nil
			update.SaveState(u.paths.dir(), st2)
		}
		return update.Manifest{}, fmt.Errorf("update: health guard failed to start, rolled back: %w", err)
	}

	return m, nil
}

// rollback restores the previously installed build (kept by the last
// swapIn under paths.previous()), records a Pending{Rollback: true} marker
// so the guard confirms it the same way it confirms a forward update, then
// starts the guard. It refuses when an update is already pending (forward
// or rollback) and when there is nothing to roll back to. It never lowers
// the version floor.
func (u updater) rollback() error {
	prevCore := filepath.Join(u.paths.previous(), "trinetra")
	fi, err := os.Stat(prevCore)
	if err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("update: no previous build to roll back to")
	}

	st, err := update.LoadState(u.paths.dir())
	if err != nil {
		return err
	}
	if st.Pending != nil {
		return fmt.Errorf("update: an update is already pending (see trinetra update status)")
	}

	prevVersion := "unknown"
	if out, rerr := u.x.Run(prevCore, "version", "--json"); rerr == nil {
		var v struct {
			Version string `json:"version"`
		}
		// Normalise a leading "v" here, same as smokeTest and
		// checkGuardHealth (update_guard.go): the guard's health check
		// compares this Pending.Version against a daemon-reported version
		// that is itself normalised, so storing it un-normalised here made
		// every rollback's health gate misreport a version mismatch and roll
		// back a perfectly healthy restart (fix-round-1 F1).
		if jerr := json.Unmarshal(out, &v); jerr == nil && v.Version != "" {
			prevVersion = strings.TrimPrefix(v.Version, "v")
		}
	}

	entries, err := os.ReadDir(u.paths.previous())
	if err != nil {
		return err
	}
	var files []string
	for _, e := range entries {
		if e.Type().IsRegular() {
			files = append(files, e.Name())
		}
	}

	st.Pending = &update.Pending{
		Version:  prevVersion,
		From:     strings.TrimPrefix(u.running.String(), "v"),
		Deadline: u.clock().Add(updateHealthDeadline).Unix(),
		Files:    files,
		Rollback: true,
	}
	if err := update.SaveState(u.paths.dir(), st); err != nil {
		return err
	}

	if err := restorePrevious(u.paths); err != nil {
		return err
	}
	return u.launchGuard()
}

// status reports the host's current self-update posture from persisted
// state plus the compiled-in trust anchors. It never fails on a missing
// state dir (LoadState treats that as empty state) and never requires
// root, so `trinetra update status --json` works for an unprivileged
// caller before any update has ever run.
func (u updater) status() (updateStatus, error) {
	st, err := update.LoadState(u.paths.dir())
	if err != nil {
		return updateStatus{}, err
	}

	previous := ""
	prevCore := filepath.Join(u.paths.previous(), "trinetra")
	if fi, err := os.Stat(prevCore); err == nil && fi.Mode().IsRegular() && u.x != nil {
		if out, rerr := u.x.Run(prevCore, "version", "--json"); rerr == nil {
			var v struct {
				Version string `json:"version"`
			}
			if jerr := json.Unmarshal(out, &v); jerr == nil {
				previous = v.Version
			}
		}
	}

	return updateStatus{
		Running:      u.running.String(),
		Floor:        st.FloorVersion(u.running).String(),
		Available:    st.Available,
		Previous:     previous,
		Pending:      st.Pending,
		Last:         st.Last,
		KeysLoaded:   keySetLoaded(u.keys),
		Fingerprints: update.Fingerprints(u.keys),
	}, nil
}

// toUpdateStatusView projects this package's own updateStatus (the
// CLI/socket-agnostic `trinetra update status` shape, built by updater.status
// plus the Channel/Source cmdUpdateStatus/core.API implementations both fill
// in from config) into core.UpdateStatusView -- the DTO core.API.UpdateStatus/
// UpdateCheck return, crossing the same trinetra->core boundary
// buildDashboardView/buildMonitoringView already establish for the
// dashboard/monitoring pages. Fingerprints is deliberately dropped: it isn't
// part of core.UpdateStatusView's contract (task-8-brief.md's interface
// list), and exposing raw key fingerprints over the control socket/web UI
// wasn't asked for.
func toUpdateStatusView(st updateStatus) core.UpdateStatusView {
	v := core.UpdateStatusView{
		Running:    st.Running,
		Channel:    st.Channel,
		Floor:      st.Floor,
		Available:  st.Available,
		Previous:   st.Previous,
		Source:     st.Source,
		KeysLoaded: st.KeysLoaded,
	}
	if st.Pending != nil {
		v.Pending = &core.UpdatePendingView{
			Version:  st.Pending.Version,
			From:     st.Pending.From,
			Deadline: st.Pending.Deadline,
			Rollback: st.Pending.Rollback,
		}
	}
	if st.Last != nil {
		v.Last = &core.UpdateResultView{
			Version: st.Last.Version,
			From:    st.Last.From,
			Outcome: st.Last.Outcome,
			Detail:  st.Last.Detail,
			At:      st.Last.At,
		}
	}
	return v
}

// coreUpdateStatus builds a core.UpdateStatusView for cfg's daemon/CLI
// process: updater.status() plus the Channel/Source cmdUpdateStatus itself
// always fills in from cfg (status() has no config of its own to read those
// from -- see its doc). Shared by inprocAPI.UpdateStatus and
// fileAPI.UpdateStatus so the two core.API backends build the exact same
// view the exact same way.
func coreUpdateStatus(c *config.Config) (core.UpdateStatusView, error) {
	st, err := newUpdater(c).status()
	if err != nil {
		return core.UpdateStatusView{}, err
	}
	st.Channel = c.UpdateChannel()
	st.Source = c.UpdateSource()
	return toUpdateStatusView(st), nil
}

// coreUpdateCheck runs `trinetra update check`'s underlying updater.check for
// cfg, then rebuilds the status view via coreUpdateStatus regardless of
// whether check itself errored (an error like ErrAlreadyInstalled or a
// verification failure still leaves a meaningful status to show -- see
// updater.check's own doc: "it returns the fetched manifest even when the
// policy check fails ... so a caller can still report what's on the
// channel"). The check error itself (if any) is still returned as this
// function's own error, unmodified.
func coreUpdateCheck(ctx context.Context, c *config.Config) (core.UpdateStatusView, error) {
	_, checkErr := newUpdater(c).check(ctx, c)
	view, statusErr := coreUpdateStatus(c)
	if statusErr != nil {
		return core.UpdateStatusView{}, statusErr
	}
	return view, checkErr
}

// keySetLoaded mirrors update.KeySet.empty() (unexported in that package)
// using its exported fields, so status() can report whether this build has
// any release keys compiled in without needing a new exported helper there.
func keySetLoaded(k update.KeySet) bool {
	return len(k.CI) > 0 && len(k.Maint) > 0 && len(k.Pointer) > 0
}

// realLaunchGuard is production launchGuard: systemd-run a detached
// `trinetra update guard` unit. Shared by newUpdater (apply/rollback launch
// the guard right after swapping a build in) and the daemon's own start hook
// (resumePendingOnStart, daemon.go: resuming a guard for a Pending update
// left over from a crash mid-apply/mid-guard), so both paths launch the
// guard identically.
func realLaunchGuard() error {
	// TRINETRA_E2E_GUARD_CMD (trinetra_testkeys builds only -- see
	// update_e2e_hooks.go/update_e2e_hooks_testkeys.go) replaces systemd-run
	// for the docker e2e harness's systemd-less host, which has no
	// systemd-run to launch a detached unit with.
	if cmd, args, ok := e2eGuardCmd(); ok {
		_, err := osExec{}.Run(cmd, args...)
		return err
	}
	_, err := osExec{}.Run("systemd-run", "--unit", "trinetra-update-guard", "--collect", "--quiet",
		"/usr/local/bin/trinetra", "update", "guard")
	return err
}

// newUpdater builds the production updater: real paths, the compiled-in
// production keys, the real (timeout-bounded) Exec, the running version
// parsed from the build stamp, and the real launchGuard (systemd-run into
// `trinetra update guard`). src is left nil so check/apply build it from c.
func newUpdater(c *config.Config) updater {
	running, _ := update.ParseVersion(version.String()) // unparsable (e.g. "dev") -> zero Version; tolerated for a dev build
	return updater{
		paths:       defaultUpdatePaths(),
		keys:        update.ProductionKeys(),
		x:           timeoutExec{smokeTestTimeout},
		now:         time.Now,
		arch:        runtime.GOARCH,
		running:     running,
		launchGuard: realLaunchGuard,
	}
}

// isRoot reports whether the process is running as root (euid 0): apply,
// rollback and guard all write BinDir/StateDir and must refuse otherwise.
func isRoot() bool { return os.Geteuid() == 0 }

// cmdUpdate dispatches `trinetra update status|check|apply|rollback|guard`.
func cmdUpdate(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: update status [--json] | check | apply [--version V] [--bundle DIR] [--channel C] [--force] | rollback | guard")
		return 2
	}
	switch args[0] {
	case "status":
		return cmdUpdateStatus(args[1:])
	case "check":
		return cmdUpdateCheck(args[1:])
	case "apply":
		return cmdUpdateApply(args[1:])
	case "rollback":
		return cmdUpdateRollback(args[1:])
	case "guard":
		return cmdUpdateGuard(args[1:])
	default:
		fmt.Fprintf(stderr, "unknown update subcommand %q\n", args[0])
		return 2
	}
}

func cmdUpdateStatus(args []string) int {
	jsonOut := false
	for _, a := range args {
		if a == "--json" {
			jsonOut = true
		}
	}
	c, err := loadCfg()
	if err != nil {
		c = config.Default()
	}
	st, err := newUpdater(c).status()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	st.Channel = c.UpdateChannel()
	st.Source = c.UpdateSource()

	if jsonOut {
		b, _ := jsonIndent(st)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	fmt.Fprintf(stdout, "running:   %s\n", st.Running)
	fmt.Fprintf(stdout, "channel:   %s\n", st.Channel)
	fmt.Fprintf(stdout, "source:    %s\n", st.Source)
	fmt.Fprintf(stdout, "floor:     %s\n", st.Floor)
	fmt.Fprintf(stdout, "available: %s\n", st.Available)
	if st.Previous != "" {
		fmt.Fprintf(stdout, "previous:  %s\n", st.Previous)
	}
	if st.Pending != nil {
		fmt.Fprintf(stdout, "pending:   %s (from %s, rollback=%v)\n", st.Pending.Version, st.Pending.From, st.Pending.Rollback)
	}
	if st.Last != nil {
		fmt.Fprintf(stdout, "last:      %s (%s)\n", st.Last.Version, st.Last.Outcome)
	}
	fmt.Fprintf(stdout, "keys loaded: %v\n", st.KeysLoaded)
	return 0
}

func cmdUpdateCheck(args []string) int {
	c, err := loadCfg()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	m, err := newUpdater(c).check(context.Background(), c)
	if err != nil {
		if errors.Is(err, update.ErrAlreadyInstalled) {
			fmt.Fprintf(stdout, "up to date: %s\n", m.Version)
			return 0
		}
		if errors.Is(err, errKnownBad) {
			fmt.Fprintf(stdout, "%s is on the channel but failed its health check here before; not offered (apply --version %s --force to retry)\n", m.Version, m.Version)
			return 0
		}
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "update available: %s (channel %s)\n", m.Version, c.UpdateChannel())
	return 0
}

func cmdUpdateApply(args []string) int {
	if !isRoot() {
		fmt.Fprintln(stderr, "must run as root")
		return 1
	}
	fs := flag.NewFlagSet("update apply", flag.ContinueOnError)
	fs.SetOutput(stderr)
	ver := fs.String("version", "", "install this exact version instead of the channel's latest")
	bundle := fs.String("bundle", "", "install from a local release bundle directory instead of the network source")
	channel := fs.String("channel", "", "override update.channel for this apply")
	force := fs.Bool("force", false, "retry a version this host previously marked bad")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	c, err := loadCfg()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	m, err := newUpdater(c).apply(context.Background(), c, applyOptions{Version: *ver, Bundle: *bundle, Channel: *channel, Force: *force})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "staged and installed %s; health guard launched (confirms within %s)\n", m.Version, updateHealthDeadline)
	return 0
}

func cmdUpdateRollback(args []string) int {
	if !isRoot() {
		fmt.Fprintln(stderr, "must run as root")
		return 1
	}
	c, err := loadCfg()
	if err != nil {
		c = config.Default()
	}
	if err := newUpdater(c).rollback(); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintln(stdout, "rolled back to the previous build; health guard launched")
	return 0
}

// cmdUpdateGuard ("trinetra update guard") is implemented in
// update_guard.go: the health-gate state machine (wait out the pending
// update's deadline, confirm or roll back, resume across a crash) that
// launchGuard starts via systemd-run right after apply/rollback swap a
// build in.
