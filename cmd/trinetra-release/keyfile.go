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
	"strings"

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

const maintKeyAAD = "trinetra-maint-key-v1"

func deriveKey(pass, salt []byte, n, r, p int) ([]byte, error) {
	return scrypt.Key(pass, salt, n, r, p, chacha20poly1305.KeySize)
}

// writeEncryptedKey generates a fresh ed25519 key, encrypts its seed with
// pass using XChaCha20-Poly1305 keyed by scrypt(pass), and writes the
// envelope to path (0600, refusing to overwrite an existing file). It
// returns the public key; the private key never touches stdout/stderr.
func writeEncryptedKey(path string, pass []byte) (ed25519.PublicKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	nonce := make([]byte, chacha20poly1305.NonceSizeX)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	const n, r, p = 1 << 15, 8, 1
	k, err := deriveKey(pass, salt, n, r, p)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(k)
	if err != nil {
		return nil, err
	}
	ct := aead.Seal(nil, nonce, priv.Seed(), []byte(maintKeyAAD))
	b, err := json.MarshalIndent(envelope{
		KDF: "scrypt", N: n, R: r, P: p,
		Salt:  base64.StdEncoding.EncodeToString(salt),
		Nonce: base64.StdEncoding.EncodeToString(nonce),
		CT:    base64.StdEncoding.EncodeToString(ct),
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Write(b); err != nil {
		return nil, err
	}
	return pub, nil
}

// readEncryptedKey decrypts a maintainer key envelope written by
// writeEncryptedKey. A wrong passphrase or corrupted file fails closed.
func readEncryptedKey(path string, pass []byte) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var e envelope
	if err := json.Unmarshal(b, &e); err != nil || e.KDF != "scrypt" {
		return nil, errors.New("not a trinetra maintainer key file")
	}
	salt, err := base64.StdEncoding.DecodeString(e.Salt)
	if err != nil {
		return nil, errors.New("not a trinetra maintainer key file")
	}
	nonce, err := base64.StdEncoding.DecodeString(e.Nonce)
	if err != nil {
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
	return ed25519.NewKeyFromSeed(seed), nil
}

// readPassphrase returns a maintainer key passphrase, either from the file
// named by TRINETRA_MAINT_PASSPHRASE_FILE (mode 0600, for automation) or by
// prompting on /dev/tty with echo disabled. It never reads argv and never
// echoes the passphrase to any log.
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

	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, errors.New("no terminal available for passphrase entry; set TRINETRA_MAINT_PASSPHRASE_FILE for automation")
	}
	defer tty.Close()

	fmt.Fprint(tty, prompt)
	if err := sttyEcho(tty, false); err != nil {
		return nil, err
	}
	defer sttyEcho(tty, true)

	line, err := bufio.NewReader(tty).ReadString('\n')
	fmt.Fprintln(tty)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return []byte(strings.TrimRight(line, "\r\n")), nil
}

// readNewPassphrase asks for a new passphrase twice on the terminal and
// requires the two entries to match; when TRINETRA_MAINT_PASSPHRASE_FILE is
// set it is read once (the file is already the single source of truth).
func readNewPassphrase() ([]byte, error) {
	if os.Getenv("TRINETRA_MAINT_PASSPHRASE_FILE") != "" {
		return readPassphrase("")
	}
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
	return p1, nil
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
