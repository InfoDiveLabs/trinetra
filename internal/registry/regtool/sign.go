package regtool

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

func prefixFor(kind string) (string, error) {
	switch kind {
	case "manifest":
		return plugin.ManifestSigPrefix, nil
	case "index":
		return plugin.IndexSigPrefix, nil
	}
	return "", fmt.Errorf("regtool: kind must be manifest or index, got %q", kind)
}

// SignBytes returns the .sig file contents for msg, or nil when kind is unknown.
func SignBytes(priv ed25519.PrivateKey, kind string, msg []byte) []byte {
	prefix, err := prefixFor(kind)
	if err != nil {
		return nil
	}
	sig := ed25519.Sign(priv, append([]byte(prefix), msg...))
	return []byte(base64.StdEncoding.EncodeToString(sig) + "\n")
}

// SignFile signs in with the seed at keyPath and writes the signature to out; kind is "manifest" or "index".
func SignFile(keyPath, kind, in, out string) error {
	if _, err := prefixFor(kind); err != nil {
		return err
	}
	priv, err := LoadKey(keyPath)
	if err != nil {
		return err
	}
	msg, err := os.ReadFile(in)
	if err != nil {
		return err
	}
	return os.WriteFile(out, SignBytes(priv, kind, msg), 0o644)
}

// VerifyFile checks that sig is a valid signature of in under pub.
func VerifyFile(pub ed25519.PublicKey, kind, in, sig string) error {
	prefix, err := prefixFor(kind)
	if err != nil {
		return err
	}
	msg, err := os.ReadFile(in)
	if err != nil {
		return err
	}
	sb, err := os.ReadFile(sig)
	if err != nil {
		return err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sb)))
	if err != nil || len(raw) != ed25519.SignatureSize {
		return plugin.ErrBadSignature
	}
	if !ed25519.Verify(pub, append([]byte(prefix), msg...), raw) {
		return plugin.ErrBadSignature
	}
	return nil
}
