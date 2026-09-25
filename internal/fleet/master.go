package fleet

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Sink is where the master puts what children send. The serverwatch package
// implements it over per-node tsfile replicas.
type Sink interface {
	// AppliedSeq is the highest seq durably applied for nodeID (0 if none).
	AppliedSeq(nodeID string) (uint64, error)
	// Apply durably applies recs (ascending seq, all > AppliedSeq) and
	// records the new applied seq before returning.
	Apply(nodeID string, recs []Record) error
	// Backfill applies unsequenced gap-repair records; the sink's own
	// ordering guard makes it idempotent.
	Backfill(nodeID string, recs []Record) error
	// Live stores the latest live view (not durable).
	Live(nodeID string, u LiveUpdate) error
}

// MasterConfig wires a Master.
type MasterConfig struct {
	CA        *CA
	Leaf      tls.Certificate
	Registry  *Registry
	Tokens    *TokenStore
	Sink      Sink
	Now       func() time.Time
	OnContact func(nodeID string, now time.Time, u *LiveUpdate)
	Logf      func(format string, args ...any)
}

// Master serves the fleet endpoints.
type Master struct {
	cfg     MasterConfig
	limiter *ipLimiter
	locksMu sync.Mutex
	locks   map[string]*sync.Mutex
	srv     *http.Server
}

// NewMaster builds a Master; nil Now/OnContact/Logf get safe defaults.
func NewMaster(cfg MasterConfig) *Master {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.OnContact == nil {
		cfg.OnContact = func(string, time.Time, *LiveUpdate) {}
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Master{cfg: cfg, limiter: newIPLimiter(5, time.Minute), locks: map[string]*sync.Mutex{}}
}

// Handler returns the fleet HTTP routes.
func (m *Master) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+PathJoin, m.handleJoin)
	mux.HandleFunc("POST "+PathRenew, m.requireNode(m.handleRenew))
	mux.HandleFunc("POST "+PathIngest, m.requireNode(m.handleIngest))
	mux.HandleFunc("POST "+PathBackfill, m.requireNode(m.handleBackfill))
	mux.HandleFunc("POST "+PathLive, m.requireNode(m.handleLive))
	return mux
}

// Serve serves TLS on ln until Shutdown.
func (m *Master) Serve(ln net.Listener) error {
	m.srv = &http.Server{
		Handler:           m.Handler(),
		TLSConfig:         ServerTLS(m.cfg.Leaf, m.cfg.CA.Cert),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	err := m.srv.ServeTLS(ln, "", "")
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown stops Serve gracefully.
func (m *Master) Shutdown(ctx context.Context) error {
	if m.srv == nil {
		return nil
	}
	return m.srv.Shutdown(ctx)
}

type nodeHandler func(w http.ResponseWriter, r *http.Request, nodeID string)

func (m *Master) requireNode(h nodeHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := NodeIDFromRequest(r)
		if !ok {
			http.Error(w, "client certificate required", http.StatusUnauthorized)
			return
		}
		if m.cfg.Registry.IsRevoked(id) {
			http.Error(w, "revoked", http.StatusForbidden)
			return
		}
		h(w, r, id)
	}
}

func (m *Master) nodeLock(id string) *sync.Mutex {
	m.locksMu.Lock()
	defer m.locksMu.Unlock()
	l, ok := m.locks[id]
	if !ok {
		l = &sync.Mutex{}
		m.locks[id] = l
	}
	return l
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func cleanName(s, id string) string {
	s = strings.TrimSpace(s)
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if utf8.RuneCountInString(s) > 64 {
		s = string([]rune(s)[:64])
	}
	if s == "" {
		s = "node-" + id[:6]
	}
	return s
}

// SignPrevKey proves possession of a child's previous key when re-joining:
// an ASN.1 ECDSA signature over SHA-256(csrPEM), base64 encoded.
func SignPrevKey(keyPEM, csrPEM []byte) (string, error) {
	key, err := parseECKeyPEM(keyPEM)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(csrPEM)
	sig, err := ecdsa.SignASN1(rand.Reader, key, sum[:])
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

func verifyPrevSig(pubB64, sigB64 string, csrPEM []byte) bool {
	der, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil {
		return false
	}
	pub, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return false
	}
	ek, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return false
	}
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return false
	}
	sum := sha256.Sum256(csrPEM)
	return ecdsa.VerifyASN1(ek, sum[:], sig)
}

func (m *Master) handleJoin(w http.ResponseWriter, r *http.Request) {
	now := m.cfg.Now()
	if !m.limiter.allow(remoteIP(r), now) {
		http.Error(w, "too many join attempts, wait a minute", http.StatusTooManyRequests)
		return
	}
	var req JoinRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	tok, err := m.cfg.Tokens.Consume(req.Token, now)
	if err != nil {
		m.cfg.Logf("fleet: join refused from %s: %v", remoteIP(r), err)
		http.Error(w, ErrTokenInvalid.Error(), http.StatusForbidden)
		return
	}

	rebind := false
	id := ""
	if req.PrevNodeID != "" {
		if prev, ok := m.cfg.Registry.Get(req.PrevNodeID); ok && !prev.Revoked && verifyPrevSig(prev.PubKey, req.PrevSig, []byte(req.CSR)) {
			id, rebind = prev.ID, true
		}
	}
	if id == "" {
		if id, err = NewNodeID(); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	certPEM, serial, pub, err := m.cfg.CA.SignClient([]byte(req.CSR), id, now, ClientCertLife)
	if err != nil {
		http.Error(w, "bad csr", http.StatusBadRequest)
		return
	}
	notAfter := now.Add(ClientCertLife).Unix()
	if rebind {
		err = m.cfg.Registry.Update(id, func(n *Node) error {
			n.PubKey, n.CertSerial, n.CertNotAfter = pub, serial, notAfter
			n.Version = req.Version
			return nil
		})
	} else {
		err = m.cfg.Registry.Add(Node{
			ID: id, Name: cleanName(req.Name, id), Tags: tok.Tags, PubKey: pub,
			CertSerial: serial, CertNotAfter: notAfter, Version: req.Version,
			Joined: now.Unix(), LastSeen: now.Unix(), RemoteAddr: r.RemoteAddr,
		})
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	m.cfg.Logf("fleet: node %s (%s) joined from %s (rebind=%v)", id, cleanName(req.Name, id), remoteIP(r), rebind)
	writeJSON(w, JoinResponse{NodeID: id, Cert: string(certPEM), CA: string(m.cfg.CA.CertPEM)})
}

func (m *Master) handleRenew(w http.ResponseWriter, r *http.Request, id string) {
	var req RenewRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	now := m.cfg.Now()
	certPEM, serial, pub, err := m.cfg.CA.SignClient([]byte(req.CSR), id, now, ClientCertLife)
	if err != nil {
		http.Error(w, "bad csr", http.StatusBadRequest)
		return
	}
	if err := m.cfg.Registry.Update(id, func(n *Node) error {
		n.PubKey, n.CertSerial, n.CertNotAfter = pub, serial, now.Add(ClientCertLife).Unix()
		return nil
	}); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, RenewResponse{Cert: string(certPEM)})
}

func (m *Master) readBatch(w http.ResponseWriter, r *http.Request) ([]Record, bool) {
	recs, err := DecodeBatch(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return nil, false
	}
	return recs, true
}

func (m *Master) handleIngest(w http.ResponseWriter, r *http.Request, id string) {
	recs, ok := m.readBatch(w, r)
	if !ok {
		return
	}
	for i := 1; i < len(recs); i++ {
		if recs[i].Seq <= recs[i-1].Seq {
			http.Error(w, "records must have strictly ascending seq", http.StatusBadRequest)
			return
		}
	}
	l := m.nodeLock(id)
	l.Lock()
	defer l.Unlock()
	applied, err := m.cfg.Sink.AppliedSeq(id)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var fresh []Record
	for _, rec := range recs {
		if rec.Seq > applied {
			fresh = append(fresh, rec)
		}
	}
	if len(fresh) > 0 {
		if err := m.cfg.Sink.Apply(id, fresh); err != nil {
			m.cfg.Logf("fleet: apply for %s failed: %v", id, err)
			http.Error(w, "apply failed", http.StatusServiceUnavailable)
			return
		}
		applied = fresh[len(fresh)-1].Seq
	}
	now := m.cfg.Now()
	m.cfg.Registry.Touch(id, now.Unix(), r.RemoteAddr, "")
	m.cfg.OnContact(id, now, nil)
	writeJSON(w, IngestResponse{AckedSeq: applied, ServerTime: now.Unix()})
}

func (m *Master) handleBackfill(w http.ResponseWriter, r *http.Request, id string) {
	recs, ok := m.readBatch(w, r)
	if !ok {
		return
	}
	l := m.nodeLock(id)
	l.Lock()
	defer l.Unlock()
	if err := m.cfg.Sink.Backfill(id, recs); err != nil {
		m.cfg.Logf("fleet: backfill for %s failed: %v", id, err)
		http.Error(w, "backfill failed", http.StatusServiceUnavailable)
		return
	}
	now := m.cfg.Now()
	m.cfg.Registry.Touch(id, now.Unix(), r.RemoteAddr, "")
	m.cfg.OnContact(id, now, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (m *Master) handleLive(w http.ResponseWriter, r *http.Request, id string) {
	var u LiveUpdate
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&u); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := m.cfg.Sink.Live(id, u); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	now := m.cfg.Now()
	m.cfg.Registry.Touch(id, now.Unix(), r.RemoteAddr, u.Version)
	m.cfg.OnContact(id, now, &u)
	writeJSON(w, map[string]int64{"server_time": now.Unix()})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, fmt.Sprint(err), http.StatusInternalServerError)
	}
}

// ipLimiter allows n events per window per IP.
type ipLimiter struct {
	mu     sync.Mutex
	n      int
	window time.Duration
	hits   map[string][]time.Time
}

func newIPLimiter(n int, window time.Duration) *ipLimiter {
	return &ipLimiter{n: n, window: window, hits: map[string][]time.Time{}}
}

func (l *ipLimiter) allow(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cut := now.Add(-l.window)
	h := l.hits[ip][:0]
	for _, t := range l.hits[ip] {
		if t.After(cut) {
			h = append(h, t)
		}
	}
	if len(h) >= l.n {
		l.hits[ip] = h
		return false
	}
	l.hits[ip] = append(h, now)
	if len(l.hits) > 10000 { // bound memory under a spray of source IPs
		for k, v := range l.hits {
			if len(v) == 0 || !v[len(v)-1].After(cut) {
				delete(l.hits, k)
			}
		}
	}
	return true
}
