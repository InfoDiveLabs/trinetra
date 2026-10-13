// cmd/trinetra-release/keyfile.go
package main

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/scrypt"
)

// envelope is the on-disk format for a passphrase-encrypted maintainer key:
// {"kdf":"scrypt","n":32768,"r":8,"p":1,"salt":b64,"nonce":b64,"ct":b64}.
type envelope struct {
	KDF   string `json:"kdf"`
	N     int    `json:"n"`
	R     int    `json:"r"`
	P     int    `json:"p"`
	Salt  string `json:"salt"`
	Nonce string `json:"nonce"`
	CT    string `json:"ct"`
}

const (
	maintKeyAAD = "trinetra-maint-key-v1"

	// envelopeN/R/P are the only scrypt parameters this tool writes. readEncryptedKey requires
	// an exact match instead of trusting N/r/p from the file.
	envelopeN       = 1 << 15
	envelopeR       = 8
	envelopeP       = 1
	envelopeSaltLen = 16
)

func deriveKey(pass, salt []byte, n, r, p int) ([]byte, error) {
	return scrypt.Key(pass, salt, n, r, p, chacha20poly1305.KeySize)
}

// writeEncryptedKey generates a fresh ed25519 key, encrypts its seed with pass using
// XChaCha20-Poly1305 keyed by scrypt(pass), and writes the envelope to path.
func writeEncryptedKey(path string, pass []byte) (ed25519.PublicKey, error) {
	if len(pass) == 0 {
		return nil, errors.New("writeEncryptedKey: empty passphrase")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, envelopeSaltLen)
	nonce := make([]byte, chacha20poly1305.NonceSizeX)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	k, err := deriveKey(pass, salt, envelopeN, envelopeR, envelopeP)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(k)
	if err != nil {
		return nil, err
	}
	ct := aead.Seal(nil, nonce, priv.Seed(), []byte(maintKeyAAD))
	b, err := json.MarshalIndent(envelope{
		KDF: "scrypt", N: envelopeN, R: envelopeR, P: envelopeP,
		Salt:  base64.StdEncoding.EncodeToString(salt),
		Nonce: base64.StdEncoding.EncodeToString(nonce),
		CT:    base64.StdEncoding.EncodeToString(ct),
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := writeKeyFile(path, b); err != nil {
		return nil, err
	}
	return pub, nil
}

// readEncryptedKey decrypts an envelope written by writeEncryptedKey.
func readEncryptedKey(path string, pass []byte) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var e envelope
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, errors.New("not a trinetra maintainer key file")
	}
	if e.KDF != "scrypt" || e.N != envelopeN || e.R != envelopeR || e.P != envelopeP {
		return nil, errors.New("not a trinetra maintainer key file (unexpected kdf parameters)")
	}
	salt, err := base64.StdEncoding.DecodeString(e.Salt)
	if err != nil || len(salt) != envelopeSaltLen {
		return nil, errors.New("not a trinetra maintainer key file")
	}
	nonce, err := base64.StdEncoding.DecodeString(e.Nonce)
	if err != nil || len(nonce) != chacha20poly1305.NonceSizeX {
		return nil, errors.New("not a trinetra maintainer key file")
	}
	ct, err := base64.StdEncoding.DecodeString(e.CT)
	if err != nil {
		return nil, errors.New("not a trinetra maintainer key file")
	}
	k, err := deriveKey(pass, salt, e.N, e.R, e.P)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(k)
	if err != nil {
		return nil, err
	}
	seed, err := aead.Open(nil, nonce, ct, []byte(maintKeyAAD))
	if err != nil {
		return nil, errors.New("wrong passphrase or corrupted key file")
	}
	if len(seed) != ed25519.SeedSize {
		return nil, errors.New("not a trinetra maintainer key file (bad seed length)")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// writeKeyFile writes data to a new file at path (0600, O_EXCL so it never overwrites a
// key), fsyncs it, and checks every error including Close.
func writeKeyFile(path string, data []byte) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(path)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return nil
}

// openTTY opens the controlling terminal for interactive prompts.
var openTTY = func() (*os.File, error) {
	return os.OpenFile("/dev/tty", os.O_RDWR, 0)
}

// readPassphrase returns a maintainer key passphrase, from the file named by
// TRINETRA_MAINT_PASSPHRASE_FILE.
func readPassphrase(prompt string) ([]byte, error) {
	if p := os.Getenv("TRINETRA_MAINT_PASSPHRASE_FILE"); p != "" {
		fi, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("TRINETRA_MAINT_PASSPHRASE_FILE: %w", err)
		}
		if fi.Mode().Perm() != 0o600 {
			return nil, fmt.Errorf("TRINETRA_MAINT_PASSPHRASE_FILE %s must be mode 0600, got %v", p, fi.Mode().Perm())
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		return bytes.TrimRight(b, "\r\n"), nil
	}

	tty, err := openTTY()
	if err != nil {
		return nil, errors.New("no terminal available for passphrase entry; set TRINETRA_MAINT_PASSPHRASE_FILE for automation")
	}
	defer tty.Close()

	fmt.Fprint(tty, prompt)
	if err := sttyEcho(tty, false); err != nil {
		return nil, err
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	restored := make(chan struct{})
	go func() {
		select {
		case <-sigCh:
			sttyEcho(tty, true)
			fmt.Fprintln(tty)
			os.Exit(130)
		case <-restored:
		}
	}()
	defer func() {
		// Stop signal delivery BEFORE releasing the watcher goroutine.
		signal.Stop(sigCh)
		close(restored)
		sttyEcho(tty, true)
	}()

	line, err := bufio.NewReader(tty).ReadString('\n')
	fmt.Fprintln(tty)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return []byte(strings.TrimRight(line, "\r\n")), nil
}

// readNewPassphrase asks twice on the terminal and requires a match; when
// TRINETRA_MAINT_PASSPHRASE_FILE is set it is read once.
func readNewPassphrase() ([]byte, error) {
	var pass []byte
	if os.Getenv("TRINETRA_MAINT_PASSPHRASE_FILE") != "" {
		p, err := readPassphrase("")
		if err != nil {
			return nil, err
		}
		pass = p
	} else {
		p1, err := readPassphrase("Passphrase: ")
		if err != nil {
			return nil, err
		}
		p2, err := readPassphrase("Confirm passphrase: ")
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(p1, p2) {
			return nil, errors.New("passphrases did not match")
		}
		pass = p1
	}
	if len(pass) == 0 {
		return nil, errors.New("passphrase must not be empty")
	}
	return pass, nil
}

func sttyEcho(tty *os.File, on bool) error {
	arg := "-echo"
	if on {
		arg = "echo"
	}
	cmd := exec.Command("stty", arg)
	cmd.Stdin = tty
	cmd.Stdout = tty
	cmd.Stderr = tty
	return cmd.Run()
}
