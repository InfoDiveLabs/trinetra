package fleet

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestPKI builds a CA and a server leaf for 127.0.0.1/localhost.
func newTestPKI(t *testing.T) (*CA, tls.Certificate) {
	t.Helper()
	now := time.Now()
	ca, err := NewCA("test fleet CA", now)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := ca.IssueServer([]string{"127.0.0.1", "localhost"}, now)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := tls.X509KeyPair(append(certPEM, ca.CertPEM...), keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return ca, leaf
}

func newTLSServer(t *testing.T, ca *CA, leaf tls.Certificate, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = ServerTLS(leaf, ca.Cert)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func clientFor(t *testing.T, pin string, cert *tls.Certificate) *http.Client {
	t.Helper()
	get := func() (*tls.Certificate, error) {
		if cert == nil {
			return &tls.Certificate{}, nil
		}
		return cert, nil
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: PinnedClientTLS(pin, get)}, Timeout: 5 * time.Second}
}

func issueClient(t *testing.T, ca *CA, nodeID string) tls.Certificate {
	t.Helper()
	keyPEM, csrPEM, err := NewKeyAndCSR(nodeID)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, serial, pub, err := ca.SignClient(csrPEM, nodeID, time.Now(), ClientCertLife)
	if err != nil {
		t.Fatal(err)
	}
	if serial == "" || pub == "" {
		t.Fatal("empty serial/pubkey")
	}
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPinIsStableAndPrefixed(t *testing.T) {
	ca, _ := newTestPKI(t)
	p1, p2 := SPKIPin(ca.Cert), SPKIPin(ca.Cert)
	if p1 != p2 || !strings.HasPrefix(p1, "sha256:") || strings.Contains(p1, "=") {
		t.Fatalf("bad pin %q / %q", p1, p2)
	}
}

func TestCASaveLoadRoundTrip(t *testing.T) {
	ca, _ := newTestPKI(t)
	dir := t.TempDir()
	cp, kp := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")
	if err := ca.Save(cp, kp); err != nil {
		t.Fatal(err)
	}
	got, err := LoadCA(cp, kp)
	if err != nil {
		t.Fatal(err)
	}
	if SPKIPin(got.Cert) != SPKIPin(ca.Cert) {
		t.Fatal("pin changed across save/load")
	}
	assertMode(t, kp, 0o600)
}

func TestPinnedClientAcceptsRightCAAndIdentifiesNode(t *testing.T) {
	ca, leaf := newTestPKI(t)
	srv := newTLSServer(t, ca, leaf, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := NodeIDFromRequest(r)
		if !ok {
			http.Error(w, "no client cert", 401)
			return
		}
		io.WriteString(w, id)
	}))
	cc := issueClient(t, ca, "node-abc")
	resp, err := clientFor(t, SPKIPin(ca.Cert), &cc).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "node-abc" {
		t.Fatalf("node id = %q", b)
	}
}

func TestPinnedClientWithoutCertStillConnects(t *testing.T) {
	ca, leaf := newTestPKI(t)
	srv := newTLSServer(t, ca, leaf, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := NodeIDFromRequest(r); ok {
			t.Error("unexpected node id without client cert")
		}
		io.WriteString(w, "ok")
	}))
	resp, err := clientFor(t, SPKIPin(ca.Cert), nil).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestPinnedClientRejectsWrongCA(t *testing.T) {
	ca, leaf := newTestPKI(t)
	other, err := NewCA("other", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	srv := newTLSServer(t, ca, leaf, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	if _, err := clientFor(t, SPKIPin(other.Cert), nil).Get(srv.URL); err == nil {
		t.Fatal("connected despite pin mismatch")
	}
}

func TestPinnedClientRejectsClientCertAsServer(t *testing.T) {
	ca, _ := newTestPKI(t)
	// A child's client-auth cert must not be usable to impersonate the master.
	cc := issueClient(t, ca, "evil")
	evil := tls.Certificate{Certificate: append(cc.Certificate, ca.Cert.Raw), PrivateKey: cc.PrivateKey}
	srv := newTLSServer(t, ca, evil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	if _, err := clientFor(t, SPKIPin(ca.Cert), nil).Get(srv.URL); err == nil {
		t.Fatal("client-auth cert accepted as server")
	}
}

func TestSignClientRejectsGarbageCSR(t *testing.T) {
	ca, _ := newTestPKI(t)
	if _, _, _, err := ca.SignClient([]byte("not a csr"), "n", time.Now(), ClientCertLife); err == nil {
		t.Fatal("garbage CSR signed")
	}
}

func TestClientCertHasClientAuthOnlyAndLife(t *testing.T) {
	ca, _ := newTestPKI(t)
	cc := issueClient(t, ca, "n1")
	c, err := x509.ParseCertificate(cc.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(c.ExtKeyUsage) != 1 || c.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Fatalf("ext key usage = %v", c.ExtKeyUsage)
	}
	if c.Subject.CommonName != "n1" {
		t.Fatalf("CN = %q", c.Subject.CommonName)
	}
	life := c.NotAfter.Sub(c.NotBefore)
	if life < ClientCertLife || life > ClientCertLife+2*time.Hour {
		t.Fatalf("life = %v", life)
	}
}
