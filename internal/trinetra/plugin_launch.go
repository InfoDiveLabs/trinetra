package trinetra

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// This file implements the safe plugin launcher: the front-door mechanism
// that lets the core `trinetra` binary exec companion binaries
// (trinetra-ctl, trinetra-web) as subcommands. Because the front-door
// commonly runs as root (via `sudo trinetra cli`), exec'ing the wrong
// file here is a privilege escalation, not just a bug. See
// plans/2026-08-01-safe-plugin-frontdoor.md for the threat model this
// defends against (PATH hijack, binary swap, tampering) and the net rule:
// a plugin is only exec'd if ALL of (absolute path derived from the core
// binary's own directory) AND (root-owned or core-owner-owned, non-writable
// file and parent dir) AND (checksum matches the install-time manifest)
// hold.
//
// Stdlib only: this file must not import anything outside the standard
// library (crypto/sha256, encoding/*, errors, fmt, io, os, path/filepath,
// syscall). TestDefaultBuildIsStdlibOnly (buildtag_test.go) enforces that
// the default (untagged) build of cmd/trinetra never pulls in
// third-party packages, and this file is part of that build.

// errPluginNotInstalled is returned when the plugin binary simply is not
// present at its expected location. Callers should print an install
// instruction (e.g. "download it" / "build it with go build ..."), not a
// security warning: a missing file is not evidence of tampering.
var errPluginNotInstalled = errors.New("trinetra: plugin not installed")

// errPluginVerificationFailed is returned when the plugin binary exists but
// fails one of the trust checks (wrong owner, writable by group/other,
// missing or mismatched manifest entry, or checksum mismatch). Callers
// should refuse to run it and print a security warning, since this may
// indicate tampering. This is deliberately a DIFFERENT sentinel from
// errPluginNotInstalled so callers (and tests) can tell "not installed" and
// "installed but unsafe" apart with errors.Is and print the right message
// for each.
var errPluginVerificationFailed = errors.New("trinetra: plugin verification failed")

// pluginPath returns the absolute, symlink-resolved path to the companion
// binary "trinetra-<name>" that must live next to the core binary
// itself. It is derived ONLY from the core binary's own location
// (filepath.Dir(os.Executable())) and NEVER consults $PATH: that is the
// defense against a PATH hijack (an attacker-writable earlier PATH entry,
// or a loosened sudo secure_path, must not be able to substitute a plugin).
//
// Any symlink in the candidate path is resolved (filepath.EvalSymlinks) so
// the checks in verifyPlugin run against the REAL target file and its REAL
// parent directory, not a symlink an attacker could repoint later.
//
// If the file does not exist, the returned error wraps
// errPluginNotInstalled so callers can distinguish "not installed" from a
// verification failure.
func pluginPath(name string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate core binary: %w", err)
	}
	dir := filepath.Dir(exe)
	candidate := filepath.Join(dir, "trinetra-"+name)

	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%w: %s", errPluginNotInstalled, candidate)
		}
		return "", fmt.Errorf("resolve plugin path %s: %w", candidate, err)
	}
	return filepath.Clean(resolved), nil
}

// verifyPlugin checks that path is genuinely safe to exec as the named
// plugin: a regular file, owned by uid 0 or expectedOwnerUID (the uid that
// owns the core binary), not group- or world-writable, with a parent
// directory that is likewise owned by uid 0 or expectedOwnerUID and not
// group- or world-writable, and whose SHA-256 matches manifest[name]. All
// of these must hold or verifyPlugin returns an error wrapping
// errPluginVerificationFailed.
//
// expectedOwnerUID and manifest are parameters (not read from global state
// or the filesystem) specifically so this function is fully unit-testable
// without root: a test can create a temp file it owns itself and pass its
// own uid as expectedOwnerUID.
//
// A missing file is reported via errPluginNotInstalled instead, since that
// is a distinct situation (nothing to refuse, just nothing installed).
func verifyPlugin(path string, expectedOwnerUID int, manifest map[string]string, name string) error {
	// Lstat, not Stat: if path were somehow still a symlink at this point
	// (pluginPath already resolves symlinks, but verifyPlugin is a
	// standalone, independently testable/callable function and must not
	// silently follow one), Mode().IsRegular() below will correctly be
	// false for a symlink and this fails closed.
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: %s", errPluginNotInstalled, path)
		}
		return fmt.Errorf("%w: stat %s: %v", errPluginVerificationFailed, path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file (mode %v)", errPluginVerificationFailed, path, info.Mode())
	}
	if err := verifyOwnerAndPerms(path, info, expectedOwnerUID); err != nil {
		return err
	}

	parent := filepath.Dir(path)
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return fmt.Errorf("%w: stat parent dir %s: %v", errPluginVerificationFailed, parent, err)
	}
	if !parentInfo.IsDir() {
		return fmt.Errorf("%w: parent %s is not a directory", errPluginVerificationFailed, parent)
	}
	if err := verifyOwnerAndPerms(parent, parentInfo, expectedOwnerUID); err != nil {
		return err
	}

	want, ok := manifest[name]
	if !ok || want == "" {
		return fmt.Errorf("%w: %s has no recorded checksum in the plugin manifest (run `trinetra install` to record it)", errPluginVerificationFailed, name)
	}
	got, err := sha256File(path)
	if err != nil {
		return fmt.Errorf("%w: checksum %s: %v", errPluginVerificationFailed, path, err)
	}
	if got != want {
		return fmt.Errorf("%w: %s checksum mismatch (expected %s, got %s); this may indicate tampering", errPluginVerificationFailed, path, want, got)
	}
	return nil
}

// verifyOwnerAndPerms is the shared owner/writability check applied to both
// the plugin file itself and its parent directory: owned by uid 0 or
// expectedOwnerUID, and not group- or world-writable (mode&0o022 == 0).
func verifyOwnerAndPerms(path string, info os.FileInfo, expectedOwnerUID int) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		// Not reachable on the unix platforms this project targets, but
		// fail closed rather than skip the check if it ever is.
		return fmt.Errorf("%w: cannot determine owner of %s on this platform", errPluginVerificationFailed, path)
	}
	uid := int(stat.Uid)
	if uid != 0 && uid != expectedOwnerUID {
		return fmt.Errorf("%w: %s is owned by uid %d, expected uid 0 or %d", errPluginVerificationFailed, path, uid, expectedOwnerUID)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%w: %s is group- or world-writable (mode %o)", errPluginVerificationFailed, path, info.Mode().Perm())
	}
	return nil
}

// sha256File returns the lowercase hex SHA-256 digest of the file at path.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// pluginManifestPath is <stateDir>/plugins.json, the root-only,
// install-time record of each companion binary's SHA-256 (written by
// cmdInstall, Task 2). It is a package-level func rather than a const so it
// picks up test overrides of the stateDir var, same as pidFile() in
// util.go.
func pluginManifestPath() string { return filepath.Join(stateDir, "plugins.json") }

// loadPluginManifest reads the install-time plugin checksum manifest. A
// missing manifest is deliberately treated as a verification FAILURE, not a
// silent pass: if the manifest is absent, verifyPlugin has nothing to check
// the checksum against, and the whole point of the manifest is that its
// absence must never be interpreted as "checks disabled".
func loadPluginManifest() (map[string]string, error) {
	path := pluginManifestPath()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: plugin manifest missing at %s (run `trinetra install` to create it)", errPluginVerificationFailed, path)
		}
		return nil, fmt.Errorf("%w: read plugin manifest %s: %v", errPluginVerificationFailed, path, err)
	}
	var manifest map[string]string
	if err := json.Unmarshal(b, &manifest); err != nil {
		return nil, fmt.Errorf("%w: parse plugin manifest %s: %v", errPluginVerificationFailed, path, err)
	}
	return manifest, nil
}

// resolveAndVerifyPlugin performs the full trust check launchPlugin runs
// before it ever calls syscall.Exec: resolve the plugin's absolute path
// (pluginPath), determine the core binary's own owner uid, load the
// install-time checksum manifest, and run verifyPlugin against all three.
// It is split out from launchPlugin specifically so this path is
// unit-testable: syscall.Exec replaces the process on success and never
// returns, so it cannot itself be exercised by a test. Everything up to
// the exec call can and is.
func resolveAndVerifyPlugin(name string) (string, error) {
	path, err := pluginPath(name)
	if err != nil {
		return "", err
	}

	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("%w: locate core binary: %v", errPluginVerificationFailed, err)
	}
	exeInfo, err := os.Lstat(exe)
	if err != nil {
		return "", fmt.Errorf("%w: stat core binary %s: %v", errPluginVerificationFailed, exe, err)
	}
	exeStat, ok := exeInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("%w: cannot determine core binary owner on this platform", errPluginVerificationFailed)
	}
	expectedOwnerUID := int(exeStat.Uid)

	manifest, err := loadPluginManifest()
	if err != nil {
		return "", err
	}

	if err := verifyPlugin(path, expectedOwnerUID, manifest, name); err != nil {
		return "", err
	}
	return path, nil
}

// launchPlugin resolves, verifies, and execs the companion binary
// "trinetra-<name>", replacing the current process image
// (syscall.Exec) so control passes to the plugin cleanly (this matters for
// the interactive `cli` subcommand, which needs the terminal handed over,
// not a child process wrapped by the core binary). socketPath and token
// are passed to the plugin via the TRINETRA_CONTROL_SOCKET and
// TRINETRA_CONTROL_TOKEN environment variables (in addition to the
// inherited environment), which is how the plugin dials the daemon's
// control socket. The old SERVERWATCH_CONTROL_SOCKET/TOKEN names are ALSO
// set, for one release, so a plugin binary built before the rename (which
// only reads the old names) still works when launched by a new core; see
// socket.go's readControlSocketEnv equivalent in each plugin, which reads
// TRINETRA_* first and falls back to SERVERWATCH_*.
//
// syscall.Exec does not return on success, so this function only ever
// returns an error: either from resolveAndVerifyPlugin (not installed, or
// verification failed -- no exec attempted in either case) or from the
// exec syscall itself failing to start.
//
// TOCTOU note: resolveAndVerifyPlugin's checksum read and this exec are not
// atomic; in principle the file could change in between. The threat model
// (see plans/2026-08-01-safe-plugin-frontdoor.md) accepts this because the
// file and its parent directory are required to be non-writable by anyone
// but uid 0 or the core binary's own owner, which makes the gap
// non-exploitable by a non-root attacker. The resolved path from
// resolveAndVerifyPlugin is reused directly for the exec (not re-resolved)
// to keep the window as small as possible.
func launchPlugin(name string, args []string, socketPath, token string) error {
	path, err := resolveAndVerifyPlugin(name)
	if err != nil {
		return err
	}

	argv := append([]string{path}, args...)
	env := append(os.Environ(),
		"TRINETRA_CONTROL_SOCKET="+socketPath,
		"TRINETRA_CONTROL_TOKEN="+token,
		// Compat: kept for one release so an old (pre-rename) plugin binary,
		// which only reads the SERVERWATCH_* names, still works when exec'd
		// by a new core. Remove once the old plugin name is no longer supported.
		"SERVERWATCH_CONTROL_SOCKET="+socketPath,
		"SERVERWATCH_CONTROL_TOKEN="+token,
	)
	return syscall.Exec(path, argv, env)
}

// launchPluginFn is the seam the front-door dispatch calls, overridable in
// tests so the dispatch's error-to-message mapping can be exercised without
// actually exec'ing a binary (syscall.Exec never returns on success).
var launchPluginFn = launchPlugin

// cmdFrontDoor resolves the control socket + token and safely execs the
// companion plugin "trinetra-<pluginName>". label is the user-facing
// subcommand ("cli"/"web") used in messages; pluginName is the binary
// suffix ("ctl"/"web"). On success launchPluginFn never returns; on failure
// it maps the sentinel error to a clear message and returns a non-zero exit
// code.
func cmdFrontDoor(label, pluginName string, args []string) int {
	socketPath := controlSocketPath()

	// Best-effort: an unreadable token file must not fail the front-door.
	// The plugin itself surfaces the auth failure if the token turns out to
	// be wrong or missing; this command's only job is to safely exec the
	// right binary.
	tokenBytes, _ := os.ReadFile(controlTokenPath())
	token := strings.TrimSpace(string(tokenBytes))

	err := launchPluginFn(pluginName, args, socketPath, token)
	if err == nil {
		// Defensive: syscall.Exec never returns on success, so this branch
		// is not normally reached, but a nil error should not be treated
		// as a failure if it somehow is.
		return 0
	}

	pluginBin := "trinetra-" + pluginName
	if errors.Is(err, errPluginNotInstalled) {
		fmt.Fprintf(stderr, "%s is not installed next to trinetra.\n", pluginBin)
		fmt.Fprintf(stderr, "Download %s from the releases page, place it next to the trinetra binary (usually /usr/local/bin/), then run `trinetra install` to record its checksum.\n", pluginBin)
		fmt.Fprintf(stderr, "Or build it from source, then run `trinetra install`:\n")
		fmt.Fprintf(stderr, "  %s\n", buildHint(pluginName))
		return 1
	}
	if errors.Is(err, errPluginVerificationFailed) {
		fmt.Fprintf(stderr, "refusing to run %s: it is not the binary this trinetra installed\n", pluginBin)
		fmt.Fprintf(stderr, "(owner/permissions/checksum check failed). This may indicate tampering.\n")
		fmt.Fprintf(stderr, "details: %v\n", err)
		return 1
	}

	fmt.Fprintf(stderr, "trinetra %s: %v\n", label, err)
	return 1
}

// buildHint returns the go build command that produces the plugin binary
// pluginName ("ctl" or "web"), used in the not-installed message so a user
// building from source has the exact command to run. Both plugins are
// plain, untagged builds from their own ./cmd directory.
func buildHint(pluginName string) string {
	if pluginName == "web" {
		return "go build -o /usr/local/bin/trinetra-web ./cmd/trinetra-web"
	}
	return "go build -o /usr/local/bin/trinetra-" + pluginName + " ./cmd/trinetra-" + pluginName
}
