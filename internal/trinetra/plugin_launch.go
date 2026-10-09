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

// This file implements the safe plugin launcher: the front-door mechanism that lets the
// core `trinetra` binary exec companion binaries.

// errPluginNotInstalled is returned when the plugin binary simply is not present at its
// expected location.
var errPluginNotInstalled = errors.New("trinetra: plugin not installed")

// errPluginVerificationFailed is returned when the plugin binary exists but fails one of
// the trust checks.
var errPluginVerificationFailed = errors.New("trinetra: plugin verification failed")

// pluginPath returns the absolute, symlink-resolved path to the companion binary
// "trinetra-<name>" that must live next to the core binary itself.
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

// verifyPlugin checks that path is genuinely safe to exec as the named plugin: a regular
// file, owned by uid 0 or expectedOwnerUID (the uid that owns the core binary).
func verifyPlugin(path string, expectedOwnerUID int, manifest map[string]string, name string) error {
	// Lstat, not Stat: if path were somehow still a symlink at this point.
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

// verifyOwnerAndPerms is the shared owner/writability check applied to both the plugin file
// itself and its parent directory: owned by uid 0 or expectedOwnerUID.
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

// pluginManifestPath is <stateDir>/plugins.json, the root-only, install-time record of each
// companion binary's SHA-256 (written by cmdInstall).
func pluginManifestPath() string { return filepath.Join(stateDir, "plugins.json") }

// loadPluginManifest reads the install-time plugin checksum manifest.
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

// resolveAndVerifyPlugin performs the full trust check launchPlugin runs before it ever
// calls syscall.Exec: resolve the plugin's absolute path (pluginPath).
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

// launchPlugin resolves, verifies, and execs the companion binary "trinetra-<name>",
// replacing the current process image.
func launchPlugin(name string, args []string, socketPath, token string) error {
	path, err := resolveAndVerifyPlugin(name)
	if err != nil {
		return err
	}

	argv := append([]string{path}, args...)
	env := append(os.Environ(),
		"TRINETRA_CONTROL_SOCKET="+socketPath,
		"TRINETRA_CONTROL_TOKEN="+token,
		// Compat: kept for one release so an old (pre-rename) plugin binary, which only reads the
		// SERVERWATCH_* names, still works when exec'd by a new core.
		"SERVERWATCH_CONTROL_SOCKET="+socketPath,
		"SERVERWATCH_CONTROL_TOKEN="+token,
	)
	return syscall.Exec(path, argv, env)
}

// launchPluginFn is the seam the front-door dispatch calls, overridable in tests so the
// dispatch's error-to-message mapping can be exercised without actually exec'ing a binary.
var launchPluginFn = launchPlugin

// cmdFrontDoor resolves the control socket + token and safely execs the companion plugin
// "trinetra-<pluginName>". label is the user-facing subcommand.
func cmdFrontDoor(label, pluginName string, args []string) int {
	socketPath := controlSocketPath()

	// Best-effort: an unreadable token file must not fail the front-door.
	tokenBytes, _ := os.ReadFile(controlTokenPath())
	token := strings.TrimSpace(string(tokenBytes))

	err := launchPluginFn(pluginName, args, socketPath, token)
	if err == nil {
		// Defensive: syscall.Exec never returns on success, so this branch is not normally
		// reached, but a nil error should not be treated as a failure if it somehow is.
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

// buildHint returns the go build command that produces the plugin binary pluginName ("ctl"
// or "web").
func buildHint(pluginName string) string {
	if pluginName == "web" {
		return "go build -o /usr/local/bin/trinetra-web ./cmd/trinetra-web"
	}
	return "go build -o /usr/local/bin/trinetra-" + pluginName + " ./cmd/trinetra-" + pluginName
}
