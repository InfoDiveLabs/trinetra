package registry

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

var testNow = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

func testPriv(b byte) ed25519.PrivateKey {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = b
	}
	return ed25519.NewKeyFromSeed(seed)
}

func testPub(b byte) ed25519.PublicKey { return testPriv(b).Public().(ed25519.PublicKey) }

func signWith(priv ed25519.PrivateKey, prefix string, msg []byte) []byte {
	sig := ed25519.Sign(priv, append([]byte(prefix), msg...))
	return []byte(base64.StdEncoding.EncodeToString(sig) + "\n")
}

func sampleIndex(serial int64) Index {
	sum := sha256.Sum256([]byte("pkg"))
	return Index{
		Schema:  IndexSchema,
		Serial:  serial,
		Issued:  testNow.Add(-time.Hour).Format(time.RFC3339),
		Expires: testNow.Add(20 * 24 * time.Hour).Format(time.RFC3339),
		Plugins: []IndexPlugin{{
			ID: "uptime", Name: "Uptime", Publisher: "InfoDive", Description: "Checks endpoints",
			Versions: []IndexVersion{
				{Version: "1.0.0", HostAPI: "^1.0", Permissions: []plugin.Permission{"metrics.write"}, Published: "2026-11-01", Packages: []IndexPackage{
					{OS: "linux", Arch: "amd64", URL: "https://example.com/uptime-1.0.0-linux-amd64.tgz", SHA256: hex.EncodeToString(sum[:]), Size: 10},
				}},
				{Version: "1.1.0", HostAPI: "^1.0", Permissions: []plugin.Permission{"metrics.write"}, Published: "2026-11-10", Packages: []IndexPackage{
					{OS: "linux", Arch: "amd64", URL: "https://example.com/uptime-1.1.0-linux-amd64.tgz", SHA256: hex.EncodeToString(sum[:]), Size: 11},
					{OS: "linux", Arch: "arm64", URL: "https://example.com/uptime-1.1.0-linux-arm64.tgz", SHA256: hex.EncodeToString(sum[:]), Size: 12},
				}},
			},
		}},
	}
}

func marshalSigned(t *testing.T, ix Index, priv ed25519.PrivateKey) (b, sig []byte) {
	t.Helper()
	b, err := json.Marshal(ix)
	if err != nil {
		t.Fatal(err)
	}
	return b, signWith(priv, plugin.IndexSigPrefix, b)
}
