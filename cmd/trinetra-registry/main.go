// Command trinetra-registry signs and assembles the Trinetra plugin registry.
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
	"github.com/InfoDiveLabs/trinetra/internal/registry"
	"github.com/InfoDiveLabs/trinetra/internal/registry/regtool"
)

const usage = `usage: trinetra-registry <command> [flags]

commands:
  keygen   -out <file>
  hash     -dir <build> -manifest plugin.json
  package  -dir <build> -os <os> -arch <arch> -out <tgz> [-unsigned]
  sign     -key <file> -kind manifest|index -in <f> -out <f.sig>
  index    -packages <dir> -base-url <url> -serial <n> -lifetime 21d [-prev <index.json>] [-revoked <revoked.json>] -out index.json
  verify   -pubkey <b64> -kind manifest|index -in <f> -sig <f.sig>
  check    -dir <plugin dir>`

var commands = map[string]func(args []string, stdout, stderr io.Writer) error{
	"keygen":  cmdKeygen,
	"hash":    cmdHash,
	"package": cmdPackage,
	"sign":    cmdSign,
	"index":   cmdIndex,
	"verify":  cmdVerify,
	"check":   cmdCheck,
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	cmd, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "trinetra-registry: unknown command %q\n%s\n", args[0], usage)
		return 2
	}
	if err := cmd(args[1:], stdout, stderr); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		fmt.Fprintf(stderr, "trinetra-registry %s: %v\n", args[0], err)
		return 1
	}
	return 0
}

func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func need(fs *flag.FlagSet, vals map[string]string) error {
	for name, v := range vals {
		if v == "" {
			return fmt.Errorf("-%s is required", name)
		}
	}
	return nil
}

func cmdKeygen(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("keygen", stderr)
	out := fs.String("out", "", "file to write the private seed to")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := need(fs, map[string]string{"out": *out}); err != nil {
		return err
	}
	pub, err := regtool.Keygen(*out)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "public: %s\nfingerprint: %s\n", base64.StdEncoding.EncodeToString(pub), regtool.Fingerprint(pub))
	return nil
}

func cmdHash(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("hash", stderr)
	dir := fs.String("dir", "", "build directory containing bin/<os>-<arch>/")
	manifest := fs.String("manifest", "plugin.json", "manifest to rewrite (relative to -dir if present there)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := need(fs, map[string]string{"dir": *dir}); err != nil {
		return err
	}
	m, err := regtool.FillHashes(*dir, *manifest)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s %s: %d binaries\n", m.ID, m.Version, len(m.Binaries))
	return nil
}

func cmdPackage(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("package", stderr)
	dir := fs.String("dir", "", "build directory")
	goos := fs.String("os", "", "target os")
	goarch := fs.String("arch", "", "target arch")
	out := fs.String("out", "", "tar.gz to write")
	unsigned := fs.Bool("unsigned", false, "package without plugin.json.sig, for sharing outside the registry (installs as unverified)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := need(fs, map[string]string{"dir": *dir, "os": *goos, "arch": *goarch, "out": *out}); err != nil {
		return err
	}
	pack := regtool.Package
	if *unsigned {
		pack = regtool.PackageUnsigned
	}
	sum, size, err := pack(*dir, *goos, *goarch, *out)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s %d\n", sum, size)
	return nil
}

func cmdSign(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("sign", stderr)
	key := fs.String("key", "", "private seed file")
	kind := fs.String("kind", "", "manifest or index")
	in := fs.String("in", "", "file to sign")
	out := fs.String("out", "", "signature file to write")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := need(fs, map[string]string{"key": *key, "kind": *kind, "in": *in, "out": *out}); err != nil {
		return err
	}
	return regtool.SignFile(*key, *kind, *in, *out)
}

func cmdIndex(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("index", stderr)
	pkgs := fs.String("packages", "", "directory of package .tgz files")
	base := fs.String("base-url", "", "https URL prefix for packages; {id} and {version} are substituted")
	serial := fs.Int64("serial", 0, "index serial (must exceed the previous index's)")
	life := fs.String("lifetime", "21d", "index lifetime")
	prev := fs.String("prev", "", "previous index.json to merge into")
	revoked := fs.String("revoked", "", "revoked.json")
	out := fs.String("out", "", "index.json to write")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := need(fs, map[string]string{"out": *out}); err != nil {
		return err
	}
	if *serial <= 0 {
		return fmt.Errorf("-serial is required")
	}
	if *pkgs != "" && *base == "" {
		return fmt.Errorf("-base-url is required with -packages")
	}
	lifetime, err := regtool.ParseLifetime(*life)
	if err != nil {
		return err
	}
	o := regtool.IndexOptions{PackagesDir: *pkgs, BaseURL: *base, Serial: *serial, Lifetime: lifetime, Now: time.Now()}
	if *prev != "" {
		b, err := os.ReadFile(*prev)
		if err != nil {
			return err
		}
		p, err := registry.DecodeIndex(b)
		if err != nil {
			return err
		}
		o.Prev = &p
	}
	if *revoked != "" {
		b, err := os.ReadFile(*revoked)
		if err != nil {
			return err
		}
		var r []plugin.Revocation
		if err := json.Unmarshal(b, &r); err != nil {
			return fmt.Errorf("%s: %w", *revoked, err)
		}
		if r == nil {
			r = []plugin.Revocation{}
		}
		o.Revoked = r
	}
	ix, raw, err := regtool.BuildIndex(o)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, raw, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "serial %d: %d plugins, expires %s\n", ix.Serial, len(ix.Plugins), ix.Expires)
	return nil
}

func cmdVerify(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("verify", stderr)
	pub := fs.String("pubkey", "", "base64 public key")
	kind := fs.String("kind", "", "manifest or index")
	in := fs.String("in", "", "signed file")
	sig := fs.String("sig", "", "signature file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := need(fs, map[string]string{"pubkey": *pub, "kind": *kind, "in": *in, "sig": *sig}); err != nil {
		return err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(*pub))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return fmt.Errorf("-pubkey is not a base64 ed25519 public key")
	}
	if err := regtool.VerifyFile(raw, *kind, *in, *sig); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "ok")
	return nil
}

func cmdCheck(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("check", stderr)
	dir := fs.String("dir", "", "plugin source directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := need(fs, map[string]string{"dir": *dir}); err != nil {
		return err
	}
	if err := regtool.Check(*dir); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "ok")
	return nil
}
