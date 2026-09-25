package fleet

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// ErrRevoked means the master refused this node's certificate.
var ErrRevoked = errors.New("fleet: this node was revoked by the master")

// JoinResult is what the caller persists into config after a join.
type JoinResult struct {
	NodeID    string
	MasterURL string
	Pin       string
}

func identityPaths(dir string) (key, cert, ca string) {
	return filepath.Join(dir, "node.key"), filepath.Join(dir, "node.crt"), filepath.Join(dir, "ca.crt")
}

// Join enrolls this host with the master named in code and writes the new
// identity into dir. A previous identity in dir is used to prove continuity so
// the master keeps the same node ID.
func Join(ctx context.Context, code, name, version string, hostinfo json.RawMessage, dir string) (JoinResult, error) {
	info, err := DecodeJoin(code)
	if err != nil {
		return JoinResult{}, err
	}
	keyPEM, csrPEM, err := NewKeyAndCSR(name)
	if err != nil {
		return JoinResult{}, err
	}
	req := JoinRequest{Token: info.Token, CSR: string(csrPEM), Name: name, Version: version, HostInfo: hostinfo}
	kp, cp, caPath := identityPaths(dir)
	if oldKey, err := os.ReadFile(kp); err == nil {
		if oldCert, err := os.ReadFile(cp); err == nil {
			if c, err := ParseCertPEM(oldCert); err == nil {
				if sig, err := SignPrevKey(oldKey, csrPEM); err == nil {
					req.PrevNodeID, req.PrevSig = c.Subject.CommonName, sig
				}
			}
		}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return JoinResult{}, err
	}
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: PinnedClientTLS(info.Pin, func() (*tls.Certificate, error) { return &tls.Certificate{}, nil })},
	}
	hr, err := http.NewRequestWithContext(ctx, "POST", info.URL+PathJoin, bytes.NewReader(body))
	if err != nil {
		return JoinResult{}, err
	}
	hr.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(hr)
	if err != nil {
		return JoinResult{}, fmt.Errorf("fleet: reach master: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return JoinResult{}, fmt.Errorf("fleet: master refused join (%d): %s", resp.StatusCode, bytes.TrimSpace(msg))
	}
	var jr JoinResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&jr); err != nil {
		return JoinResult{}, err
	}
	if _, err := tls.X509KeyPair([]byte(jr.Cert), keyPEM); err != nil {
		return JoinResult{}, fmt.Errorf("fleet: master returned an unusable certificate: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return JoinResult{}, err
	}
	if err := writeFileAtomic(kp, keyPEM, 0o600); err != nil {
		return JoinResult{}, err
	}
	if err := writeFileAtomic(cp, []byte(jr.Cert), 0o644); err != nil {
		return JoinResult{}, err
	}
	if err := writeFileAtomic(caPath, []byte(jr.CA), 0o644); err != nil {
		return JoinResult{}, err
	}
	return JoinResult{NodeID: jr.NodeID, MasterURL: info.URL, Pin: info.Pin}, nil
}

// Identity is a child's current key and cert, swappable on renewal.
type Identity struct {
	dir  string
	cert atomic.Pointer[tls.Certificate]
	leaf atomic.Pointer[x509.Certificate]
}

// LoadIdentity reads the identity Join wrote.
func LoadIdentity(dir string) (*Identity, error) {
	id := &Identity{dir: dir}
	kp, cp, _ := identityPaths(dir)
	c, err := tls.LoadX509KeyPair(cp, kp)
	if err != nil {
		return nil, fmt.Errorf("fleet: load identity: %w", err)
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return nil, err
	}
	id.cert.Store(&c)
	id.leaf.Store(leaf)
	return id, nil
}

// NodeID is the CN of the current cert.
func (id *Identity) NodeID() string { return id.leaf.Load().Subject.CommonName }

// Cert returns the current client certificate.
func (id *Identity) Cert() (*tls.Certificate, error) { return id.cert.Load(), nil }

// NotAfter is the current cert's expiry.
func (id *Identity) NotAfter() time.Time { return id.leaf.Load().NotAfter }

// NeedsRenew reports whether 2/3 of the cert lifetime has passed.
func (id *Identity) NeedsRenew(now time.Time) bool {
	l := id.leaf.Load()
	life := l.NotAfter.Sub(l.NotBefore)
	return now.After(l.NotBefore.Add(life * 2 / 3))
}

// Renew rotates the key and cert over the existing mTLS connection.
func (id *Identity) Renew(ctx context.Context, c *http.Client, masterURL string) error {
	keyPEM, csrPEM, err := NewKeyAndCSR(id.NodeID())
	if err != nil {
		return err
	}
	body, err := json.Marshal(RenewRequest{CSR: string(csrPEM)})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", masterURL+PathRenew, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		return ErrRevoked
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fleet: renew status %d", resp.StatusCode)
	}
	var rr RenewResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&rr); err != nil {
		return err
	}
	pair, err := tls.X509KeyPair([]byte(rr.Cert), keyPEM)
	if err != nil {
		return err
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return err
	}
	kp, cp, _ := identityPaths(id.dir)
	if err := writeFileAtomic(kp, keyPEM, 0o600); err != nil {
		return err
	}
	if err := writeFileAtomic(cp, []byte(rr.Cert), 0o644); err != nil {
		return err
	}
	id.cert.Store(&pair)
	id.leaf.Store(leaf)
	return nil
}

// GapFiller rebuilds records for a dropped outbox range from local history.
type GapFiller interface {
	Fill(g Gap) ([]Record, error)
}

// ShipperConfig wires a Shipper.
type ShipperConfig struct {
	MasterURL string
	Pin       string
	Identity  *Identity
	Outbox    *Outbox
	Gaps      GapFiller
	Live      func() (LiveUpdate, error)
	LiveEvery time.Duration
	Now       func() time.Time
	Logf      func(format string, args ...any)
}

// LinkStatus is the child's view of its link to the master.
type LinkStatus struct {
	State     string      `json:"state"`
	LastAck   int64       `json:"last_ack,omitempty"`
	LastError string      `json:"last_error,omitempty"`
	Outbox    OutboxStats `json:"outbox"`
}

// gapFillMaxAttempts bounds how many consecutive GapFiller.Fill failures for
// the same gap are tolerated before it is abandoned (resolved without being
// repaired), so a permanently-broken local rebuild cannot block newer data
// from ever reaching the master.
const gapFillMaxAttempts = 5

// Shipper moves outbox records and live updates to the master.
type Shipper struct {
	cfg     ShipperConfig
	client  *http.Client
	revoked atomic.Bool
	mu      sync.Mutex
	st      LinkStatus

	// backoff computes the data-loop retry delay for a given attempt count.
	// It defaults to backoffDelay; tests may shorten it so retry-heavy
	// scenarios (e.g. gap-fill abandonment) run in milliseconds instead of
	// real wall-clock backoff.
	backoff func(attempt int) time.Duration

	// gapFailSeq/gapFailN track consecutive GapFiller.Fill failures for the
	// gap currently being repaired, keyed by its FirstSeq: reset to the new
	// gap's key (and 0) whenever the oldest gap changes, and to 0 whenever
	// Fill succeeds. Only ever touched from the single dataLoop goroutine.
	gapFailSeq uint64
	gapFailN   int
}

// NewShipper builds a Shipper.
func NewShipper(cfg ShipperConfig) *Shipper {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.LiveEvery <= 0 {
		cfg.LiveEvery = 5 * time.Second
	}
	return &Shipper{
		cfg: cfg,
		client: &http.Client{
			Timeout:   60 * time.Second,
			Transport: &http.Transport{TLSClientConfig: PinnedClientTLS(cfg.Pin, cfg.Identity.Cert), ForceAttemptHTTP2: true},
		},
		st:      LinkStatus{State: "connecting"},
		backoff: backoffDelay,
	}
}

// Status returns the current link status.
func (s *Shipper) Status() LinkStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.st
	st.Outbox = s.cfg.Outbox.Stats()
	return st
}

func (s *Shipper) setOK() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.State != "revoked" {
		s.st.State = "linked"
	}
	s.st.LastAck = s.cfg.Now().Unix()
	s.st.LastError = ""
}

func (s *Shipper) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if errors.Is(err, ErrRevoked) {
		s.st.State = "revoked"
	} else if s.st.State != "revoked" {
		s.st.State = "retrying"
	}
	s.st.LastError = err.Error()
}

// retryAfterError carries a server-requested minimum wait.
type retryAfterError struct {
	wait time.Duration
	msg  string
}

func (e *retryAfterError) Error() string { return e.msg }

// Run ships until ctx is done or the node is revoked.
func (s *Shipper) Run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s.liveLoop(ctx) }()
	go func() { defer wg.Done(); s.renewLoop(ctx) }()
	s.dataLoop(ctx)
	cancel()
	wg.Wait()
}

func backoffDelay(attempt int) time.Duration {
	ceil := time.Second << min(attempt, 6) // 1s .. 64s
	if ceil > 60*time.Second {
		ceil = 60 * time.Second
	}
	return time.Second + time.Duration(rand.Int64N(int64(ceil)))
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (s *Shipper) dataLoop(ctx context.Context) {
	attempt := 0
	for ctx.Err() == nil && !s.revoked.Load() {
		worked, err := s.shipOnce(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.setErr(err)
			if errors.Is(err, ErrRevoked) {
				s.revoked.Store(true)
				s.cfg.Logf("fleet: %v; shipping stopped, data kept locally", err)
				return
			}
			d := s.backoff(attempt)
			var ra *retryAfterError
			if errors.As(err, &ra) && ra.wait > d {
				d = ra.wait
			}
			attempt++
			if !sleepCtx(ctx, d) {
				return
			}
			continue
		}
		attempt = 0
		if worked {
			continue
		}
		t := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-s.cfg.Outbox.Notify():
		case <-t.C:
		}
		t.Stop()
	}
}

// shipOnce does one unit of work: repair the oldest gap, or send one batch.
// It reports whether it made progress (true) so dataLoop knows whether to
// immediately look for more work or fall back to waiting for one.
func (s *Shipper) shipOnce(ctx context.Context) (bool, error) {
	if gaps := s.cfg.Outbox.Gaps(); len(gaps) > 0 {
		g := gaps[0]
		if s.gapFailSeq != g.FirstSeq {
			s.gapFailSeq, s.gapFailN = g.FirstSeq, 0
		}
		var recs []Record
		if s.cfg.Gaps != nil {
			var err error
			if recs, err = s.cfg.Gaps.Fill(g); err != nil {
				s.gapFailN++
				if s.gapFailN < gapFillMaxAttempts {
					// A transient local failure: report it as an error so
					// dataLoop backs off and retries the same gap, instead
					// of losing it permanently on the first hiccup.
					return false, fmt.Errorf("fleet: gap %d-%d fill attempt %d/%d failed: %w", g.FirstSeq, g.LastSeq, s.gapFailN, gapFillMaxAttempts, err)
				}
				s.cfg.Logf("fleet: gap %d-%d abandoned after %d failed local rebuilds: %v", g.FirstSeq, g.LastSeq, s.gapFailN, err)
				recs = nil
			} else {
				s.gapFailN = 0
			}
		} else {
			s.cfg.Logf("fleet: gap %d-%d dropped (no local history to repair from)", g.FirstSeq, g.LastSeq)
		}
		for len(recs) > 0 {
			n := batchSize(recs)
			if _, err := s.post(ctx, PathBackfill, recs[:n]); err != nil {
				return false, err
			}
			recs = recs[n:]
		}
		if err := s.cfg.Outbox.ResolveGap(g.FirstSeq); err != nil {
			return false, err
		}
		s.gapFailSeq, s.gapFailN = 0, 0
		s.setOK()
		return true, nil
	}
	recs, err := s.cfg.Outbox.Read(s.cfg.Outbox.Acked(), MaxBatchBytes, MaxBatchRecords)
	if err != nil {
		return false, err
	}
	if len(recs) == 0 {
		// Nothing queued; a live update may still be keeping the link warm.
		return false, nil
	}
	body, err := s.post(ctx, PathIngest, recs)
	if err != nil {
		return false, err
	}
	var ir IngestResponse
	if err := json.Unmarshal(body, &ir); err != nil {
		return false, fmt.Errorf("fleet: bad ingest response: %w", err)
	}
	if err := s.cfg.Outbox.Ack(ir.AckedSeq); err != nil {
		var div *DivergenceError
		if !errors.As(err, &div) {
			return false, err
		}
		// Already repaired by Ack (gap recorded, numbering moved past the
		// master's); say so loudly and carry on.
		s.cfg.Logf("fleet: WARNING %v", div)
	}
	s.setOK()
	return true, nil
}

// batchSize returns how many of recs's leading elements fit within
// MaxBatchRecords and MaxBatchBytes of Data, mirroring the cap Outbox.Read
// already applies on the ingest path. It always returns at least 1 (if recs
// is non-empty) so a single oversized record still makes progress instead of
// stalling forever.
func batchSize(recs []Record) int {
	n, total := 0, 0
	for _, r := range recs {
		if n > 0 && (n >= MaxBatchRecords || total+len(r.Data) > MaxBatchBytes) {
			break
		}
		total += len(r.Data)
		n++
	}
	return n
}

func (s *Shipper) post(ctx context.Context, path string, recs []Record) ([]byte, error) {
	b, err := EncodeBatch(recs)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", s.cfg.MasterURL+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set(HeaderSentAt, strconv.FormatInt(s.cfg.Now().Unix(), 10))
	return s.do(req)
}

func (s *Shipper) do(req *http.Request) ([]byte, error) {
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusForbidden:
		return nil, ErrRevoked
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable:
		secs, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
		return nil, &retryAfterError{wait: time.Duration(secs) * time.Second, msg: fmt.Sprintf("fleet: master busy (%d)", resp.StatusCode)}
	case resp.StatusCode/100 != 2:
		return nil, fmt.Errorf("fleet: master returned %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	return body, nil
}

// liveTimeout bounds how long liveLoop waits for one cfg.Live() call:
// LiveEvery, but never more than 30s and never less than 1s.
func liveTimeout(every time.Duration) time.Duration {
	t := every
	if t > 30*time.Second {
		t = 30 * time.Second
	}
	if t < time.Second {
		t = time.Second
	}
	return t
}

func (s *Shipper) liveLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.LiveEvery)
	defer t.Stop()
	timeout := liveTimeout(s.cfg.LiveEvery)
	for {
		if s.revoked.Load() {
			return
		}
		if s.cfg.Live != nil {
			s.liveOnce(ctx, timeout)
			if ctx.Err() != nil || s.revoked.Load() {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// liveOnce runs one live-update tick. cfg.Live takes no context, so it is
// called in a goroutine and raced against ctx and timeout: a callback that
// hangs (or a master that never responds) skips this tick instead of
// wedging Run's shutdown forever. If cfg.Live never returns, that one
// goroutine is abandoned rather than killed -- unavoidable given its
// signature, and harmless since it can only happen once per stuck call.
func (s *Shipper) liveOnce(ctx context.Context, timeout time.Duration) {
	type result struct {
		u   LiveUpdate
		err error
	}
	ch := make(chan result, 1)
	go func() {
		u, err := s.cfg.Live()
		ch <- result{u, err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var r result
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
		s.cfg.Logf("fleet: live snapshot callback did not return within %s, skipping this tick", timeout)
		return
	case r = <-ch:
	}
	if r.err != nil {
		return
	}
	u := r.u
	u.SentAt = s.cfg.Now().Unix()
	u.Outbox = s.cfg.Outbox.Stats()
	b, err := json.Marshal(u)
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, "POST", s.cfg.MasterURL+PathLive, bytes.NewReader(b))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if _, err := s.do(req); err != nil {
		if ctx.Err() != nil {
			return
		}
		s.setErr(err)
		if errors.Is(err, ErrRevoked) {
			s.revoked.Store(true)
		}
		return
	}
	s.setOK()
}

func (s *Shipper) renewLoop(ctx context.Context) {
	for {
		if s.cfg.Identity.NeedsRenew(s.cfg.Now()) {
			if err := s.cfg.Identity.Renew(ctx, s.client, s.cfg.MasterURL); err != nil {
				s.cfg.Logf("fleet: certificate renewal failed (expires %s): %v", s.cfg.Identity.NotAfter().Format(time.RFC3339), err)
			} else {
				s.cfg.Logf("fleet: certificate renewed, now valid until %s", s.cfg.Identity.NotAfter().Format(time.RFC3339))
			}
		}
		if !sleepCtx(ctx, time.Hour) {
			return
		}
	}
}
