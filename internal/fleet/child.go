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

// JoinResult is what the caller persists into config after a join. Name is
// the name actually stored on the master (which may differ from what was
// requested -- see JoinResponse's doc comment), for the CLI to print.
type JoinResult struct {
	NodeID    string
	Name      string
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
	return JoinResult{NodeID: jr.NodeID, Name: jr.Name, MasterURL: info.URL, Pin: info.Pin}, nil
}

// Identity is a child's current key and cert, swappable on renewal.
type Identity struct {
	dir  string
	cert atomic.Pointer[tls.Certificate]
	leaf atomic.Pointer[x509.Certificate]
}

// Renew stages the new pair as node.key.new/node.crt.new before moving it
// into place, so a crash can never leave a key and cert that do not match.
const stagedSuffix = ".new"

func loadPair(certPath, keyPath string) (*tls.Certificate, *x509.Certificate, error) {
	c, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, nil, err
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return nil, nil, err
	}
	return &c, leaf, nil
}

// LoadIdentity reads the identity Join wrote, finishing an interrupted
// renewal first: once the master has signed a renewal it only honours the
// new certificate, so a staged pair that is newer than the installed one (or
// the only one that loads) is promoted, as is a staged cert whose key was
// already moved into place when the crash hit.
func LoadIdentity(dir string) (*Identity, error) {
	kp, cp, _ := identityPaths(dir)
	c, leaf, err := loadPair(cp, kp)
	if nc, nleaf, nerr := loadPair(cp+stagedSuffix, kp+stagedSuffix); nerr == nil && (err != nil || nleaf.NotBefore.After(leaf.NotBefore)) {
		if perr := promoteStaged(kp); perr != nil {
			return nil, perr
		}
		if perr := promoteStaged(cp); perr != nil {
			return nil, perr
		}
		c, leaf, err = nc, nleaf, nil
	} else if err != nil {
		if nc, nleaf, nerr := loadPair(cp+stagedSuffix, kp); nerr == nil {
			if perr := promoteStaged(cp); perr != nil {
				return nil, perr
			}
			c, leaf, err = nc, nleaf, nil
		}
	}
	if err != nil {
		return nil, fmt.Errorf("fleet: load identity: %w", err)
	}
	id := &Identity{dir: dir}
	id.cert.Store(c)
	id.leaf.Store(leaf)
	return id, nil
}

func promoteStaged(path string) error {
	if err := os.Rename(path+stagedSuffix, path); err != nil {
		return fmt.Errorf("fleet: finish interrupted certificate renewal: %w", err)
	}
	return nil
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
	// Stage both files durably, then rename them into place. A crash at any
	// point leaves either the old pair intact plus a complete staged pair,
	// or the new key installed with the new cert still staged; LoadIdentity
	// finishes the job either way. Writing node.key then node.crt in place
	// could leave a key that does not match its cert.
	kp, cp, _ := identityPaths(id.dir)
	if err := writeFileAtomic(kp+stagedSuffix, keyPEM, 0o600); err != nil {
		return err
	}
	if err := writeFileAtomic(cp+stagedSuffix, []byte(rr.Cert), 0o644); err != nil {
		return err
	}
	if err := promoteStaged(kp); err != nil {
		return err
	}
	if err := promoteStaged(cp); err != nil {
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
	// OnFrame, if set, starts a stream client goroutine in Run that hands every
	// non-ping frame to OnFrame. Nil means no stream client.
	// OnFrame runs synchronously on the stream's read loop: it must not block, or
	// it stalls reading and trips the read-idle watchdog (streamIdleTimeout).
	OnFrame func(Frame)
}

// Link states reported in LinkStatus.State.
const (
	LinkConnecting = "connecting"  // nothing has reached the master yet
	LinkLinked     = "linked"      // the master is reachable and data is flowing
	LinkCatchingUp = "catching up" // live updates reach the master, but the data lane is retrying with records still unsent
	LinkRetrying   = "retrying"    // the master is unreachable or refusing requests
	LinkRevoked    = "revoked"     // the master revoked this node
)

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
	cfg    ShipperConfig
	client *http.Client
	// streamClient serves PathStream. It shares client's pinned-TLS HTTP/2
	// Transport but sets no Timeout, which caps the whole exchange and would kill a
	// healthy long-lived stream. Liveness is enforced by streamOnce's read-idle
	// watchdog instead.
	streamClient *http.Client
	revoked      atomic.Bool
	mu           sync.Mutex
	st           LinkStatus // LastAck; State and LastError are derived in Status
	// Per-lane outcome of the most recent attempt: "" before the first,
	// laneOK after a success, otherwise the error text.
	dataLane, liveLane string

	// backoff computes the data-loop retry delay; tests shorten it.
	backoff func(attempt int) time.Duration

	// gapFailSeq/gapFailN count consecutive GapFiller.Fill failures for the gap
	// being repaired (keyed by FirstSeq), reset when the oldest gap changes or Fill
	// succeeds. Only touched from the dataLoop goroutine.
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
	transport := &http.Transport{TLSClientConfig: PinnedClientTLS(cfg.Pin, cfg.Identity.Cert), ForceAttemptHTTP2: true}
	return &Shipper{
		cfg: cfg,
		client: &http.Client{
			Timeout:   60 * time.Second,
			Transport: transport,
		},
		streamClient: &http.Client{
			// No Timeout: see the field comment on Shipper.streamClient.
			Transport: transport,
		},
		st:      LinkStatus{State: LinkConnecting},
		backoff: defaultBackoff,
	}
}

// laneOK marks a lane whose last attempt succeeded.
const laneOK = "ok"

// Status returns the current link status. The state combines both lanes: it
// is "linked" only while the data lane's last attempt succeeded (or nothing
// is waiting to be sent), and "catching up" when live updates get through
// but the data lane is backing off with records still unsent.
func (s *Shipper) Status() LinkStatus {
	ob := s.cfg.Outbox.Stats()
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.st
	st.Outbox = ob
	dataFailing := s.dataLane != "" && s.dataLane != laneOK
	liveFailing := s.liveLane != "" && s.liveLane != laneOK
	switch {
	case s.revoked.Load() || st.State == LinkRevoked:
		st.State = LinkRevoked
	case dataFailing && ob.Unacked == 0 && ob.Gaps == 0 && !liveFailing:
		st.State = LinkLinked // nothing waiting on the failed lane
	case dataFailing && s.liveLane == laneOK:
		st.State = LinkCatchingUp
	case dataFailing || liveFailing:
		st.State = LinkRetrying
	case s.dataLane == laneOK || s.liveLane == laneOK:
		st.State = LinkLinked
	}
	switch {
	case dataFailing:
		st.LastError = s.dataLane
	case liveFailing:
		st.LastError = s.liveLane
	}
	return st
}

// setOK records a successful attempt on a lane (data when data is true).
func (s *Shipper) setOK(data bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if data {
		s.dataLane = laneOK
	} else {
		s.liveLane = laneOK
	}
	s.st.LastAck = s.cfg.Now().Unix()
}

// setErr records a failed attempt on a lane.
func (s *Shipper) setErr(data bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if errors.Is(err, ErrRevoked) {
		s.st.State = LinkRevoked
	}
	if data {
		s.dataLane = err.Error()
	} else {
		s.liveLane = err.Error()
	}
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
	if s.cfg.OnFrame != nil {
		wg.Add(1)
		go func() { defer wg.Done(); s.streamLoop(ctx) }()
	}
	s.dataLoop(ctx)
	cancel()
	wg.Wait()
}

// backoffBase is backoffDelay's unit: attempt 0 waits in [backoffBase,
// 2*backoffBase), doubling per attempt up to 60*backoffBase. A var so tests can
// scale the jittered shape down.
var backoffBase = time.Second

func backoffDelay(attempt int) time.Duration {
	ceil := backoffBase << min(attempt, 6) // backoffBase .. 64*backoffBase
	if ceilCap := 60 * backoffBase; ceil > ceilCap {
		ceil = ceilCap
	}
	return backoffBase + time.Duration(rand.Int64N(int64(ceil)))
}

// defaultBackoff is the jittered exponential backoff every new Shipper starts
// with (streamLoop reuses it). A var so tests can shorten it.
var defaultBackoff = backoffDelay

// maxBackoffAttempt is where backoffDelay's cap saturates; streamLoop waits that
// long against an old master with no stream endpoint, which will not change
// until the master is upgraded.
const maxBackoffAttempt = 6

// errStreamNotFound means the master has no PathStream route (an old master).
var errStreamNotFound = errors.New("fleet: master has no /fleet/v1/stream endpoint (old master)")

// errStreamIdle means no frame, pings included, arrived within
// streamIdleTimeout: the connection is presumed dead.
var errStreamIdle = errors.New("fleet: stream read timed out waiting for a frame (pings included)")

// streamIdleTimeout is how long streamOnce reads nothing, pings included, before
// reconnecting. Pings keep it from firing on a healthy idle link.
func streamIdleTimeout() time.Duration { return 3 * pingIntervalDuration() }

// streamIdleCheckInterval is how often the watchdog polls; it can be loose since
// streamIdleTimeout has 3x the ping cadence of slack.
func streamIdleCheckInterval() time.Duration {
	iv := pingIntervalDuration()
	if iv <= 0 {
		return time.Second
	}
	return iv
}

// streamLoop reconnects to PathStream, handing frames to cfg.OnFrame, until ctx
// is done or the node is revoked. It shares Shipper.backoff with the data loop.
func (s *Shipper) streamLoop(ctx context.Context) {
	attempt := 0
	loggedOldMaster := false
	for ctx.Err() == nil && !s.revoked.Load() {
		established, err := s.streamOnce(ctx)
		if ctx.Err() != nil {
			return // clean shutdown: streamOnce only returns a nil error here
		}
		// Here err is non-nil: streamOnce returns nil only when ctx was cancelled,
		// which the check above caught.
		if errors.Is(err, ErrRevoked) {
			s.revoked.Store(true)
			s.cfg.Logf("fleet: %v; stream stopped", err)
			return
		}
		if errors.Is(err, errStreamNotFound) {
			if !loggedOldMaster {
				s.cfg.Logf("fleet: %v", err)
				loggedOldMaster = true
			}
			if !sleepCtx(ctx, s.backoff(maxBackoffAttempt)) {
				return
			}
			continue
		}
		// A connection that read at least one frame before ending is no sign the master
		// is struggling: reconnect at the first-attempt delay.
		if established {
			attempt = 0
		}
		d := s.backoff(attempt)
		attempt++
		if !sleepCtx(ctx, d) {
			return
		}
	}
}

// streamOnce opens PathStream and reads frames until the connection ends.
// established reports whether at least one frame was read, which separates a
// struggling master (keep backing off) from a healthy connection that simply
// ended (reconnect promptly). A nil error means ctx is done.
func (s *Shipper) streamOnce(ctx context.Context) (established bool, err error) {
	// streamCtx lets the watchdog abort a stuck read without cancelling ctx.
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()

	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, s.cfg.MasterURL+PathStream, nil)
	if err != nil {
		return false, err
	}
	resp, err := s.streamClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return false, nil
		}
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusForbidden:
		return false, ErrRevoked
	case http.StatusNotFound:
		return false, errStreamNotFound
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return false, fmt.Errorf("fleet: stream status %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}

	// Watchdog: if nothing is read for streamIdleTimeout, cancel streamCtx so the
	// blocked Decode returns instead of hanging on a silently dead connection.
	var lastRead atomic.Int64
	lastRead.Store(time.Now().UnixNano())
	watchdogDone := make(chan struct{})
	defer close(watchdogDone)
	go func() {
		t := time.NewTicker(streamIdleCheckInterval())
		defer t.Stop()
		for {
			select {
			case <-watchdogDone:
				return
			case <-t.C:
				if time.Since(time.Unix(0, lastRead.Load())) > streamIdleTimeout() {
					cancelStream()
					return
				}
			}
		}
	}()

	dec := json.NewDecoder(resp.Body)
	for {
		var f Frame
		if err := dec.Decode(&f); err != nil {
			if ctx.Err() != nil {
				return established, nil
			}
			if streamCtx.Err() != nil {
				// Only our watchdog cancels streamCtx; ctx itself is not done (checked above).
				return established, errStreamIdle
			}
			return established, err
		}
		established = true
		lastRead.Store(time.Now().UnixNano())
		switch f.Type {
		case "ping":
			continue
		case "revoked":
			return established, ErrRevoked
		default:
			s.cfg.OnFrame(f)
		}
	}
}

// PostRPCResult reports the result of an RPC the master pushed over the
// stream: id is the rpc frame's id, body is the raw result to record.
func (s *Shipper) PostRPCResult(ctx context.Context, id string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.MasterURL+PathRPC+id, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	_, err = s.do(req)
	return err
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
			s.setErr(true, err)
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

// shipOnce repairs the oldest gap or sends one batch, reporting whether it made
// progress.
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
					// A transient local failure: return an error so dataLoop backs off and retries
					// the gap instead of losing it.
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
		s.setOK(true)
		return true, nil
	}

	// Priority lane: ship unacked KindAlert records via Backfill (unsequenced)
	// before the general backlog, so a fired alert reaches the master promptly even
	// behind a huge samples backlog. Backfill never advances the master's AppliedSeq
	// and this call never Acks, so the ordinary batch below is unaffected; when it
	// reaches the alert's seq, Ingest applies and Acks it, dropping it from the
	// priority index. Until then every call re-sends it, which the master's replica
	// alert guard (dedup by time + line) makes free.
	prioritySent := false
	precs, err := s.cfg.Outbox.ReadPriority(MaxBatchBytes, MaxBatchRecords)
	if err != nil {
		return false, err
	}
	if len(precs) > 0 {
		if _, err := s.post(ctx, PathBackfill, precs); err != nil {
			return false, err
		}
		s.setOK(true)
		prioritySent = true
	}

	recs, err := s.cfg.Outbox.Read(s.cfg.Outbox.Acked(), MaxBatchBytes, MaxBatchRecords)
	if err != nil {
		// The priority send made durable progress even though the backlog read failed,
		// so report that rather than "no progress" (shipOnce's contract).
		return prioritySent, err
	}
	if len(recs) == 0 {
		// Nothing queued; a live update may still be keeping the link warm.
		return prioritySent, nil
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
		// Already repaired by Ack (gap recorded, numbering moved past the master's).
		s.cfg.Logf("fleet: WARNING %v", div)
	}
	s.setOK(true)
	return true, nil
}

// batchSize returns how many leading recs fit MaxBatchRecords and MaxBatchBytes
// of Data, like Outbox.Read. It returns at least 1 so an oversized record still
// makes progress.
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

// liveOnce runs one live tick. cfg.Live takes no context, so it runs in a
// goroutine raced against ctx and timeout: a hung callback skips the tick instead
// of wedging shutdown. The stuck goroutine is abandoned, which is unavoidable.
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
		s.setErr(false, err)
		if errors.Is(err, ErrRevoked) {
			s.revoked.Store(true)
		}
		return
	}
	s.setOK(false)
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
