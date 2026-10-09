// Command trinetra-release is the maintainer-only tool for building,
// signing, and co-signing trinetra release manifests and channel pointers.
// It is not part of the shipped core binary and may depend on non-stdlib
// crypto packages; see internal/trinetra/buildtag_test.go for the stdlib
// guarantee that binds cmd/trinetra instead.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/update"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// signMaintTest signs with the e2e-fixture maintainer test key
// (updatetest.NewTestSigner(2)).
var signMaintTest func(in, out string) error

// testKeySet returns the deterministic test trust anchor (updatetest.TestKeySet) for
// `verify --testkeys` and `cosign --testkeys`.
var testKeySet func() update.KeySet

// errTestKeysUnavailable is returned by every --testkeys path in a default build.
var errTestKeysUnavailable = errors.New("--testkeys is only available in a trinetra_testkeys build")

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: trinetra-release <keygen|manifest|sign|pointer|latest|verify|fingerprints|cosign> [args]")
		return 2
	}
	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "keygen":
		err = cmdKeygen(rest)
	case "manifest":
		err = cmdManifest(rest)
	case "sign":
		err = cmdSign(rest)
	case "pointer":
		err = cmdPointer(rest)
	case "latest":
		err = cmdLatest(rest)
	case "verify":
		err = cmdVerify(rest)
	case "fingerprints":
		err = cmdFingerprints(rest)
	case "cosign":
		err = cmdCosign(rest)
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "trinetra-release:", err)
		return 1
	}
	return 0
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	return fs
}

// printPublicKey prints only the public key and its fingerprint.
func printPublicKey(role string, pub ed25519.PublicKey) {
	sum := sha256.Sum256(pub)
	fmt.Printf("public: %s\n", base64.StdEncoding.EncodeToString(pub))
	fmt.Printf("fingerprint: %s:%s\n", role, hex.EncodeToString(sum[:]))
}

// cmdKeygen implements: keygen --role ci|maint|pointer --out FILE
func cmdKeygen(args []string) error {
	fs := newFlagSet("keygen")
	role := fs.String("role", "", "ci, maint, or pointer")
	out := fs.String("out", "", "output key file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("keygen: --out is required")
	}
	switch *role {
	case "ci", "pointer":
		pub, err := writeSeedKey(*out)
		if err != nil {
			return err
		}
		printPublicKey(*role, pub)
		return nil
	case "maint":
		pass, err := readNewPassphrase()
		if err != nil {
			return err
		}
		pub, err := writeEncryptedKey(*out, pass)
		if err != nil {
			return err
		}
		printPublicKey(*role, pub)
		return nil
	default:
		return fmt.Errorf("keygen: unknown --role %q (want ci, maint, or pointer)", *role)
	}
}

// writeSeedKey generates a fresh ed25519 key and writes its base64 seed to path (0600,
// refusing to overwrite an existing file), for pasting into a GitHub Actions secret.
func writeSeedKey(path string) (ed25519.PublicKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	line := base64.StdEncoding.EncodeToString(priv.Seed()) + "\n"
	if err := writeKeyFile(path, []byte(line)); err != nil {
		return nil, err
	}
	return pub, nil
}

// releaseStems and releaseArches define the exact release set: three binaries times three
// linux architectures, nine files.
var (
	releaseStems  = []string{"trinetra", "trinetra-ctl", "trinetra-web"}
	releaseArches = []string{"amd64", "arm64", "arm"}
)

// expectedReleaseFiles returns the exact set of "stem-linux-arch" names a
// release must contain, mapped to (os, arch) for building each File entry.
func expectedReleaseFiles() map[string][2]string {
	want := make(map[string][2]string, len(releaseStems)*len(releaseArches))
	for _, stem := range releaseStems {
		for _, arch := range releaseArches {
			want[stem+"-linux-"+arch] = [2]string{"linux", arch}
		}
	}
	return want
}

// cmdManifest implements: manifest --dir DIR --version V --channel C --min-upgrade-from V
// --published RFC3339 [--keys-from-binary | --keys-ci ...
func cmdManifest(args []string) error {
	fs := newFlagSet("manifest")
	dir := fs.String("dir", "", "directory containing the release files")
	version := fs.String("version", "", "release version")
	channel := fs.String("channel", "", "release channel")
	minUpgradeFrom := fs.String("min-upgrade-from", "", "minimum running version allowed to upgrade directly")
	published := fs.String("published", "", "RFC3339 publish timestamp")
	keysCI := fs.String("keys-ci", "", "comma-separated base64 CI public keys")
	keysMaint := fs.String("keys-maint", "", "comma-separated base64 maintainer public keys")
	keysPointer := fs.String("keys-pointer", "", "comma-separated base64 pointer public keys")
	keysFromBinary := fs.Bool("keys-from-binary", false, "fill keys with this tool's compiled-in production key set (same commit as the binaries)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	manifestKeys := update.ManifestKeys{CI: splitCSV(*keysCI), Maint: splitCSV(*keysMaint), Pointer: splitCSV(*keysPointer)}
	if *keysFromBinary {
		if *keysCI != "" || *keysMaint != "" || *keysPointer != "" {
			return errors.New("manifest: --keys-from-binary cannot be combined with --keys-ci/--keys-maint/--keys-pointer")
		}
		k := update.ProductionKeys()
		if len(k.CI) == 0 || len(k.Maint) == 0 || len(k.Pointer) == 0 {
			return errors.New("manifest: --keys-from-binary: this build has no release keys compiled in")
		}
		manifestKeys = keysToB64(k)
	}
	if *dir == "" || *version == "" || *channel == "" || *minUpgradeFrom == "" || *published == "" {
		return errors.New("manifest: --dir, --version, --channel, --min-upgrade-from, and --published are required")
	}
	entries, err := os.ReadDir(*dir)
	if err != nil {
		return err
	}
	expected := expectedReleaseFiles()
	seen := make(map[string]bool, len(expected))
	var files []update.File
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "trinetra") || !strings.Contains(name, "-linux-") {
			continue // not a release file at all: not considered, per brief.
		}
		osArch, ok := expected[name]
		if !ok {
			return fmt.Errorf("manifest: unexpected release file %q in %s (want one of %v)", name, *dir, sortedKeys(expected))
		}
		size, sum, err := hashFile(filepath.Join(*dir, name))
		if err != nil {
			return err
		}
		files = append(files, update.File{Name: name, OS: osArch[0], Arch: osArch[1], Size: size, SHA256: sum})
		seen[name] = true
	}
	if len(seen) != len(expected) {
		var missing []string
		for name := range expected {
			if !seen[name] {
				missing = append(missing, name)
			}
		}
		sort.Strings(missing)
		return fmt.Errorf("manifest: missing release file(s) in %s: %s", *dir, strings.Join(missing, ", "))
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })

	m := update.Manifest{
		Schema:         1,
		Product:        "trinetra",
		Version:        *version,
		Channel:        *channel,
		Published:      *published,
		MinUpgradeFrom: *minUpgradeFrom,
		Keys:           manifestKeys,
		Files:          files,
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if _, err := update.DecodeManifest(b); err != nil {
		return fmt.Errorf("manifest: generated manifest is invalid: %w", err)
	}
	return os.WriteFile(filepath.Join(*dir, "manifest.json"), b, 0o644)
}

// keysToB64 renders a key set the way manifest.keys carries it.
func keysToB64(k update.KeySet) update.ManifestKeys {
	enc := func(in []update.PublicKey) []string {
		out := make([]string, len(in))
		for i, pk := range in {
			out[i] = base64.StdEncoding.EncodeToString(pk)
		}
		return out
	}
	return update.ManifestKeys{CI: enc(k.CI), Maint: enc(k.Maint), Pointer: enc(k.Pointer)}
}

func sortedKeys(m map[string][2]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func hashFile(path string) (int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// cmdSign implements: sign --role ci|pointer|maint-test --in FILE --out FILE
func cmdSign(args []string) error {
	fs := newFlagSet("sign")
	role := fs.String("role", "", "ci, pointer, or maint-test (testkeys build only)")
	in := fs.String("in", "", "input file")
	out := fs.String("out", "", "output signature file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *in == "" || *out == "" {
		return errors.New("sign: --in and --out are required")
	}

	if *role == "maint-test" {
		if signMaintTest == nil {
			return errors.New("sign: --role maint-test is only available in a trinetra_testkeys build")
		}
		return signMaintTest(*in, *out)
	}

	var prefix string
	switch *role {
	case "ci":
		prefix = update.ReleasePrefix
	case "pointer":
		prefix = update.ChannelPrefix
	default:
		return fmt.Errorf("sign: unknown --role %q (want ci or pointer)", *role)
	}
	seedB64 := os.Getenv("TRINETRA_SIGNING_KEY")
	if seedB64 == "" {
		return errors.New("sign: TRINETRA_SIGNING_KEY is not set")
	}
	priv, err := privFromSeedB64(seedB64)
	if err != nil {
		return fmt.Errorf("sign: TRINETRA_SIGNING_KEY: %w", err)
	}
	msg, err := os.ReadFile(*in)
	if err != nil {
		return err
	}
	sig := signWithPrefix(priv, prefix, msg)
	return os.WriteFile(*out, sig, 0o644)
}

// signWithPrefix matches the wire format produced by updatetest.TestSigner:
// base64(ed25519.Sign(priv, prefix||msg)) followed by a newline.
func signWithPrefix(priv ed25519.PrivateKey, prefix string, msg []byte) []byte {
	sig := ed25519.Sign(priv, append([]byte(prefix), msg...))
	return []byte(base64.StdEncoding.EncodeToString(sig) + "\n")
}

func privFromSeedB64(s string) (ed25519.PrivateKey, error) {
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("invalid base64 seed")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// cmdPointer implements: pointer --channel C --version V --issued RFC3339
// --out FILE, with expires set to exactly issued + 14 days.
func cmdPointer(args []string) error {
	fs := newFlagSet("pointer")
	channel := fs.String("channel", "", "channel")
	version := fs.String("version", "", "version")
	issued := fs.String("issued", "", "RFC3339 issued timestamp")
	out := fs.String("out", "", "output pointer file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *channel == "" || *version == "" || *issued == "" || *out == "" {
		return errors.New("pointer: --channel, --version, --issued, and --out are required")
	}
	issuedTime, err := time.Parse(time.RFC3339, *issued)
	if err != nil {
		return fmt.Errorf("pointer: --issued: %w", err)
	}
	p := update.Pointer{
		Schema:  1,
		Product: "trinetra",
		Channel: *channel,
		Version: *version,
		Issued:  issuedTime.Format(time.RFC3339),
		Expires: issuedTime.Add(update.MaxPointerLifetime).Format(time.RFC3339),
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(*out, b, 0o644)
}

// cmdVerify implements: verify DIR [--testkeys]
func cmdVerify(args []string) error {
	testkeys := false
	var dirs []string
	for _, a := range args {
		switch a {
		case "--testkeys", "-testkeys":
			testkeys = true
		default:
			dirs = append(dirs, a)
		}
	}
	if len(dirs) != 1 {
		return errors.New("verify: expected exactly one directory argument")
	}
	keys := update.ProductionKeys()
	if testkeys {
		if testKeySet == nil {
			return fmt.Errorf("verify: %w", errTestKeysUnavailable)
		}
		keys = testKeySet()
	}
	m, err := verifyDir(dirs[0], keys)
	if err != nil {
		return fmt.Errorf("verify: %w", err)
	}
	if testkeys {
		// Loud and impossible to mistake for a production result: a workflow glancing at
		// the last line must not read this as "the real release keys checked out".
		fmt.Println("WARNING: verified against TEST keys, not production keys")
	}
	fmt.Printf("verified %s %s (published %s) - %d files ok\n", m.Version, m.Channel, m.Published, len(m.Files))
	return nil
}

// verifyDir verifies DIR's manifest.json against manifest.ci.sig and
// manifest.maint.sig, then checks every listed file's size and sha256.
func verifyDir(dir string, keys update.KeySet) (update.Manifest, error) {
	manifest, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return update.Manifest{}, err
	}
	ciSig, err := os.ReadFile(filepath.Join(dir, "manifest.ci.sig"))
	if err != nil {
		return update.Manifest{}, err
	}
	maintSig, err := os.ReadFile(filepath.Join(dir, "manifest.maint.sig"))
	if err != nil {
		return update.Manifest{}, err
	}
	m, err := update.VerifyRelease(keys, manifest, ciSig, maintSig)
	if err != nil {
		return update.Manifest{}, err
	}
	for _, f := range m.Files {
		size, sum, err := hashFile(filepath.Join(dir, f.Name))
		if err != nil {
			return update.Manifest{}, fmt.Errorf("%s: %w", f.Name, err)
		}
		if size != f.Size {
			return update.Manifest{}, fmt.Errorf("%s: size mismatch: manifest %d, actual %d", f.Name, f.Size, size)
		}
		if sum != f.SHA256 {
			return update.Manifest{}, fmt.Errorf("%s: sha256 mismatch", f.Name)
		}
	}
	return m, nil
}

// cmdFingerprints implements: fingerprints
func cmdFingerprints(args []string) error {
	for _, fp := range update.Fingerprints(update.ProductionKeys()) {
		fmt.Println(fp)
	}
	return nil
}
