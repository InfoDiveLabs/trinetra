package fleet

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"time"
)

// Certificate lifetimes. The CA is long-lived because v1 has no CA rotation;
// children pin it. Leaves are short enough that renewal is exercised.
const (
	CALife         = 10 * 365 * 24 * time.Hour
	ServerCertLife = 2 * 365 * 24 * time.Hour
	ClientCertLife = 90 * 24 * time.Hour
	// clockSkewGrace backdates NotBefore so a child whose clock is slightly
	// behind the master's can still use a freshly issued cert.
	clockSkewGrace = time.Hour
)

// CA is the master's private certificate authority.
type CA struct {
	Cert    *x509.Certificate
	Key     *ecdsa.PrivateKey
	CertPEM []byte
}

func randSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}

// NewCA generates a fresh ECDSA P-256 CA.
func NewCA(commonName string, now time.Time) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName, Organization: []string{"serverwatch fleet"}},
		NotBefore:             now.Add(-clockSkewGrace),
		NotAfter:              now.Add(CALife),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key, CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}, nil
}

// ParseCertPEM parses the first CERTIFICATE block in b.
func ParseCertPEM(b []byte) (*x509.Certificate, error) {
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			return nil, errors.New("fleet: no CERTIFICATE PEM block")
		}
		if blk.Type == "CERTIFICATE" {
			return x509.ParseCertificate(blk.Bytes)
		}
	}
}

func parseECKeyPEM(b []byte) (*ecdsa.PrivateKey, error) {
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "EC PRIVATE KEY" {
		return nil, errors.New("fleet: no EC PRIVATE KEY PEM block")
	}
	return x509.ParseECPrivateKey(blk.Bytes)
}

func ecKeyPEM(k *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

// LoadCA reads a CA saved by Save.
func LoadCA(certPath, keyPath string) (*CA, error) {
	cb, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	kb, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	cert, err := ParseCertPEM(cb)
	if err != nil {
		return nil, err
	}
	key, err := parseECKeyPEM(kb)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key, CertPEM: cb}, nil
}

// Save writes the CA cert (0644) and key (0600) atomically.
func (ca *CA) Save(certPath, keyPath string) error {
	kp, err := ecKeyPEM(ca.Key)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(keyPath, kp, 0o600); err != nil {
		return err
	}
	return writeFileAtomic(certPath, ca.CertPEM, 0o644)
}

// IssueServer issues the master's TLS leaf for hosts (DNS names or IPs).
func (ca *CA) IssueServer(hosts []string, now time.Time) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := randSerial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "serverwatch fleet master"},
		NotBefore:    now.Add(-clockSkewGrace),
		NotAfter:     now.Add(ServerCertLife),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if h != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		return nil, nil, err
	}
	kp, err := ecKeyPEM(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), kp, nil
}

// parseCSR decodes and fully validates csrPEM: well-formed PEM, a parseable
// CSR, a self-signature that checks out, and an ECDSA key. It is the single
// place that validation lives; both CheckCSR and SignClient call it so a
// caller can validate a CSR before spending anything (e.g. a join token) and
// SignClient never has to re-check what its caller already checked.
func parseCSR(csrPEM []byte) (*x509.CertificateRequest, error) {
	blk, _ := pem.Decode(csrPEM)
	if blk == nil || blk.Type != "CERTIFICATE REQUEST" {
		return nil, errors.New("fleet: no CERTIFICATE REQUEST PEM block")
	}
	csr, err := x509.ParseCertificateRequest(blk.Bytes)
	if err != nil {
		return nil, err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("fleet: csr signature: %w", err)
	}
	if _, ok := csr.PublicKey.(*ecdsa.PublicKey); !ok {
		return nil, errors.New("fleet: csr key must be ECDSA")
	}
	return csr, nil
}

// CheckCSR validates csrPEM (PEM, parse, signature, ECDSA key) without
// issuing anything. Callers that must not spend a resource (like a
// single-use join token) on a malformed CSR call this first.
func CheckCSR(csrPEM []byte) error {
	_, err := parseCSR(csrPEM)
	return err
}

// SignClient validates csrPEM and issues a client-auth-only cert with
// CN=nodeID. It returns the cert PEM, the hex serial, and the base64 PKIX
// public key (recorded in the registry for re-bind proofs).
func (ca *CA) SignClient(csrPEM []byte, nodeID string, now time.Time, life time.Duration) ([]byte, string, string, error) {
	csr, err := parseCSR(csrPEM)
	if err != nil {
		return nil, "", "", err
	}
	serial, err := randSerial()
	if err != nil {
		return nil, "", "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: nodeID},
		NotBefore:    now.Add(-clockSkewGrace),
		NotAfter:     now.Add(life),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, csr.PublicKey, ca.Key)
	if err != nil {
		return nil, "", "", err
	}
	pub, err := x509.MarshalPKIXPublicKey(csr.PublicKey)
	if err != nil {
		return nil, "", "", err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		hex.EncodeToString(serial.Bytes()), base64.StdEncoding.EncodeToString(pub), nil
}

// NewKeyAndCSR generates a child keypair and a CSR for it. The private key
// never leaves the child.
func NewKeyAndCSR(commonName string) (keyPEM, csrPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: commonName}}, key)
	if err != nil {
		return nil, nil, err
	}
	kp, err := ecKeyPEM(key)
	if err != nil {
		return nil, nil, err
	}
	return kp, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}

// SPKIPin is "sha256:" + unpadded base64url of SHA-256(SubjectPublicKeyInfo).
func SPKIPin(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return "sha256:" + base64.RawURLEncoding.EncodeToString(sum[:])
}

// PinnedClientTLS trusts exactly one CA: the one in the server's presented
// chain whose SPKI pin equals pin. System roots are ignored and the hostname
// is not checked (the private CA only ever issues one server-auth leaf, and
// the ServerAuth EKU check below stops a child's client cert impersonating
// the master), so a master reached by IP or a renamed host still verifies.
// getCert supplies the client certificate (may return an empty one during
// join, before the child has a cert).
func PinnedClientTLS(pin string, getCert func() (*tls.Certificate, error)) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // replaced by VerifyConnection below
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("fleet: server sent no certificate")
			}
			var ca *x509.Certificate
			for _, c := range cs.PeerCertificates {
				if c.IsCA && SPKIPin(c) == pin {
					ca = c
					break
				}
			}
			if ca == nil {
				return errors.New("fleet: master CA does not match the pinned fingerprint")
			}
			roots := x509.NewCertPool()
			roots.AddCert(ca)
			_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{
				Roots:     roots,
				KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			})
			return err
		},
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return getCert()
		},
	}
}

// ServerTLS serves leaf (whose chain must include the CA cert so children can
// pin it) and verifies client certs against ca when presented. Client certs
// are optional at the TLS layer because /fleet/v1/join has none yet; every
// other endpoint requires one via NodeIDFromRequest.
func ServerTLS(leaf tls.Certificate, ca *x509.Certificate) *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{leaf},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    pool,
	}
}

// NodeIDFromRequest returns the CN of the verified client certificate.
func NodeIDFromRequest(r *http.Request) (string, bool) {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.VerifiedChains[0]) == 0 {
		return "", false
	}
	cn := r.TLS.VerifiedChains[0][0].Subject.CommonName
	return cn, cn != ""
}
