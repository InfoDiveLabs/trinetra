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
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Sink is where the master puts what children send. The trinetra package
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
	CA       *CA
	Leaf     tls.Certificate
	Registry *Registry
	Tokens   *TokenStore
	Sink     Sink
	// Hub fans lease/receipt/silence/managed-config/rpc frames out over
	// GET PathStream and receives RPC results posted to PathRPC. A nil Hub
	// gets a default built with Logf.
	Hub       *Hub
	Now       func() time.Time
	OnContact func(nodeID string, now time.Time, u *LiveUpdate)
	// OnSkew receives the child's send time (unix seconds) for every request
	// that carries one (HeaderSentAt on ingest/backfill, SentAt on live), so
	// the caller can track clock skew as now - sentAt. now is when the
	// request arrived, before it was applied. Optional.
	OnSkew func(nodeID string, now time.Time, sentAt int64)
	Logf   func(format string, args ...any)
}

// Master serves the fleet endpoints.
type Master struct {
	cfg     MasterConfig
	limiter *ipLimiter
	locksMu sync.Mutex
	locks   map[string]*sync.Mutex
	srv     *http.Server
}

// NewMaster builds a Master; nil Now/OnContact/Logf get safe defaults. The
// *http.Server is built here and never reassigned, so Serve and Shutdown
// only ever read/call a field set once before any goroutine starts -- no
// lock or nil check is needed at the call sites, and Shutdown is safe to
// call even if Serve is never called (or hasn't been called yet).
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
	if cfg.OnSkew == nil {
		cfg.OnSkew = func(string, time.Time, int64) {}
	}
	if cfg.Hub == nil {
		cfg.Hub = NewHub(cfg.Logf)
	}
	m := &Master{cfg: cfg, limiter: newIPLimiter(5, time.Minute, defaultIPLimiterCap), locks: map[string]*sync.Mutex{}}
	m.srv = &http.Server{
		Handler:           m.Handler(),
		TLSConfig:         ServerTLS(cfg.Leaf, cfg.CA.Cert),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	return m
}

// Handler returns the fleet HTTP routes.
func (m *Master) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+PathJoin, m.handleJoin)
	mux.HandleFunc("POST "+PathRenew, m.requireNode(m.handleRenew))
	mux.HandleFunc("POST "+PathIngest, m.requireNode(m.handleIngest))
	mux.HandleFunc("POST "+PathBackfill, m.requireNode(m.handleBackfill))
	mux.HandleFunc("POST "+PathLive, m.requireNode(m.handleLive))
	mux.HandleFunc("GET "+PathStream, m.requireNode(m.handleStream))
	mux.HandleFunc("POST "+PathRPC+"{id}", m.requireNode(m.handleRPC))
	return mux
}

// Serve serves TLS on ln until Shutdown. The server was already built by
// NewMaster, so this only starts it -- Serve does not mutate m.
func (m *Master) Serve(ln net.Listener) error {
	err := m.srv.ServeTLS(ln, "", "")
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown stops Serve gracefully. Safe to call even if Serve was never
// called or hasn't been called yet: http.Server.Shutdown on a server with no
// active listeners or connections returns immediately.
func (m *Master) Shutdown(ctx context.Context) error {
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
		n, known := m.cfg.Registry.Get(id)
		if !known || n.Revoked {
			http.Error(w, "revoked", http.StatusForbidden)
			return
		}
		cert, _ := ClientCertFromRequest(r)
		switch serial := CertSerialHex(cert); {
		case n.CertSerial == "" || serial == n.CertSerial:
			if n.PrevCertSerial != "" {
				// The renewed cert is in use: the old one is now superseded.
				_ = m.cfg.Registry.Update(id, func(n *Node) error {
					if serial == n.CertSerial {
						n.PrevCertSerial = ""
					}
					return nil
				})
			}
		case serial == n.PrevCertSerial:
			// Renewed but the node has not switched yet (e.g. the renew
			// response was lost): still honoured until it does.
		default:
			m.cfg.Logf("fleet: refused superseded certificate %s for node %s", serial, id)
			http.Error(w, "certificate superseded", http.StatusForbidden)
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
	// Validate the CSR before spending the single-use token: a malformed
	// CSR must not burn a token the child can still retry with.
	if err := CheckCSR([]byte(req.CSR)); err != nil {
		http.Error(w, "bad csr", http.StatusBadRequest)
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
			// A re-bind supersedes every earlier certificate at once.
			n.PubKey, n.CertSerial, n.PrevCertSerial, n.CertNotAfter = pub, serial, "", notAfter
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
	// The name actually stored may differ from what was requested (Add
	// suffixes a case-insensitive collision with "-2", "-3", ...): re-fetch
	// it so the join response -- and the log line -- report the real name.
	finalName := cleanName(req.Name, id)
	if n, ok := m.cfg.Registry.Get(id); ok {
		finalName = n.Name
	}
	m.cfg.Logf("fleet: node %s (%s) joined from %s (rebind=%v)", id, finalName, remoteIP(r), rebind)
	writeJSON(w, JoinResponse{NodeID: id, Name: finalName, Cert: string(certPEM), CA: string(m.cfg.CA.CertPEM)})
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
	presented := ""
	if c, ok := ClientCertFromRequest(r); ok {
		presented = CertSerialHex(c)
	}
	if err := m.cfg.Registry.Update(id, func(n *Node) error {
		// Keep the certificate this request came in with acceptable until
		// the node uses the new one.
		n.PrevCertSerial = presented
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
	arrived := m.cfg.Now()
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
	m.noteSentAt(id, arrived, r)
	writeJSON(w, IngestResponse{AckedSeq: applied, ServerTime: now.Unix()})
}

func (m *Master) handleBackfill(w http.ResponseWriter, r *http.Request, id string) {
	arrived := m.cfg.Now()
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
	m.noteSentAt(id, arrived, r)
	w.WriteHeader(http.StatusNoContent)
}

func (m *Master) handleLive(w http.ResponseWriter, r *http.Request, id string) {
	arrived := m.cfg.Now()
	var u LiveUpdate
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&u); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := m.cfg.Sink.Live(id, u); err != nil {
		m.cfg.Logf("fleet: live update for %s failed: %v", id, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	now := m.cfg.Now()
	m.cfg.Registry.Touch(id, now.Unix(), r.RemoteAddr, u.Version)
	m.cfg.OnContact(id, now, &u)
	if u.SentAt > 0 {
		m.cfg.OnSkew(id, arrived, u.SentAt)
	}
	writeJSON(w, map[string]int64{"server_time": now.Unix()})
}

// noteSentAt reports the request's HeaderSentAt, if present and valid, to
// OnSkew. arrived is when the request reached the handler: time the master
// then spends applying it is not the child's clock being off.
func (m *Master) noteSentAt(id string, now time.Time, r *http.Request) {
	if v, err := strconv.ParseInt(r.Header.Get(HeaderSentAt), 10, 64); err == nil && v > 0 {
		m.cfg.OnSkew(id, now, v)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, fmt.Sprint(err), http.StatusInternalServerError)
	}
}

// defaultIPLimiterCap bounds the number of distinct source IPs an ipLimiter
// tracks at once, so a spray of join attempts from many different IPs cannot
// grow its map without bound.
const defaultIPLimiterCap = 10000

// ipLimiter allows n events per window per IP, tracking at most cap distinct
// IPs at a time.
type ipLimiter struct {
	mu     sync.Mutex
	n      int
	window time.Duration
	cap    int
	hits   map[string][]time.Time
}

func newIPLimiter(n int, window time.Duration, cap int) *ipLimiter {
	return &ipLimiter{n: n, window: window, cap: cap, hits: map[string][]time.Time{}}
}

func (l *ipLimiter) allow(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cut := now.Add(-l.window)

	if hits, tracked := l.hits[ip]; tracked {
		fresh := hits[:0]
		for _, t := range hits {
			if t.After(cut) {
				fresh = append(fresh, t)
			}
		}
		if len(fresh) >= l.n {
			l.hits[ip] = fresh
			return false
		}
		l.hits[ip] = append(fresh, now)
		return true
	}

	// A new IP. Bound the map: sweep out IPs whose most recent hit is
	// stale, and only if the map is still at/over cap after sweeping do we
	// refuse -- fail closed rather than let it grow unbounded under a
	// sustained spray of distinct active IPs.
	if len(l.hits) >= l.cap {
		for k, v := range l.hits {
			if len(v) == 0 || !v[len(v)-1].After(cut) {
				delete(l.hits, k)
			}
		}
		if len(l.hits) >= l.cap {
			return false
		}
	}
	l.hits[ip] = []time.Time{now}
	return true
}
