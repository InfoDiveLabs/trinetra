package trinetra

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
)

const publicCacheTTL = 5 * time.Second

type statusPageRuntime struct {
	mu     sync.Mutex
	dir    string
	role   func() string
	getCfg func() *config.Config
	gather func(now time.Time) statusInputs
	echoFn func(text string, channels []string)
	logf   func(string, ...any)
	now    func() time.Time

	data  *statusPageData
	im    *incidentManager
	evals []core.ServiceEvaluation

	pubAt  time.Time
	pubVal core.PublicStatus
}

var _ core.StatusPageAPI = (*statusPageRuntime)(nil)

func newStatusPageRuntime(dir string, role func() string, getCfg func() *config.Config,
	gather func(now time.Time) statusInputs, echo func(text string, channels []string),
	logf func(string, ...any)) *statusPageRuntime {
	r := &statusPageRuntime{dir: dir, role: role, getCfg: getCfg, gather: gather, echoFn: echo, logf: logf, now: time.Now}
	r.data = loadStatusPage(dir, logf)
	r.im = &incidentManager{
		data:        r.data,
		now:         func() time.Time { return r.now() },
		autoResolve: func() time.Duration { return r.getCfg().StatusAutoResolveAfter() },
		echo:        r.echo,
		newID:       randomStatusID,
	}
	return r
}

func (r *statusPageRuntime) echo(inc core.StatusIncident, u core.IncidentUpdate) {
	if r.echoFn == nil {
		return
	}
	chans := r.getCfg().Status.EchoChannels
	if len(chans) == 0 {
		return
	}
	text := fmt.Sprintf("📣 Public status — %s [%s]: %s", inc.Title, titleWord(u.Status), u.Message)
	// Synchronous by contract: the daemon's echoFn itself hands off to a
	// goroutine, so posting never blocks on a channel send.
	r.echoFn(text, slices.Clone(chans))
}

func (r *statusPageRuntime) guard() error {
	if r.role() == config.RoleChild {
		return core.ErrStatusPageOnChild
	}
	return nil
}

// persistLocked saves and invalidates the public cache. Caller holds r.mu.
func (r *statusPageRuntime) persistLocked() error {
	r.pubAt = time.Time{}
	if err := r.data.save(r.dir); err != nil {
		r.logf("status page: save failed: %v", err)
		return err
	}
	return nil
}

// titleWord upper-cases the first byte; all incident statuses are ASCII.
func titleWord(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// Tick runs one evaluation with gathered inputs (test helper).
func (r *statusPageRuntime) Tick(now time.Time) {
	if r.guard() != nil || r.gather == nil {
		return
	}
	r.TickWith(now, r.gather(now))
}

// TickWith is Tick with caller-built inputs (the daemon's sampler goroutine
// owns the alert state and snapshot it gathers from).
func (r *statusPageRuntime) TickWith(now time.Time, in statusInputs) {
	if r.guard() != nil {
		return
	}
	in.Now = now
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.data.Services) == 0 && len(r.data.Incidents) == 0 {
		return
	}
	changes, evals := evaluateServices(r.data.Services, r.data.State, in)
	r.evals = evals
	r.im.applyChanges(changes)
	r.im.tickAutoResolve()
	r.data.prune(now)
	_ = r.persistLocked()
}

func (r *statusPageRuntime) Services() ([]core.StatusService, error) {
	if err := r.guard(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := slices.Clone(r.data.Services)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Order < out[j].Order })
	return out, nil
}

func (r *statusPageRuntime) SetService(s core.StatusService, actor string) (core.StatusService, error) {
	if err := r.guard(); err != nil {
		return s, err
	}
	s.Name, s.Group = strings.TrimSpace(s.Name), strings.TrimSpace(s.Group)
	if err := core.ValidateStatusService(s); err != nil {
		return s, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if i := slices.IndexFunc(r.data.Services, func(x core.StatusService) bool { return x.ID == s.ID }); i >= 0 {
		r.data.Services[i] = s
	} else {
		r.data.Services = append(r.data.Services, s)
	}
	return s, r.persistLocked()
}

func (r *statusPageRuntime) DeleteService(id, actor string) error {
	if err := r.guard(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	i := slices.IndexFunc(r.data.Services, func(x core.StatusService) bool { return x.ID == id })
	if i < 0 {
		return fmt.Errorf("no such service %q: %w", id, core.ErrNotFound)
	}
	r.data.Services = slices.Delete(r.data.Services, i, i+1)
	delete(r.data.State, id)
	return r.persistLocked()
}

func (r *statusPageRuntime) Evaluation() ([]core.ServiceEvaluation, error) {
	if err := r.guard(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.evals), nil
}

func (r *statusPageRuntime) Incidents(includeResolved bool) ([]core.StatusIncident, error) {
	if err := r.guard(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []core.StatusIncident
	for _, inc := range r.data.Incidents {
		if includeResolved || inc.Status != core.IncidentResolved {
			out = append(out, inc)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Opened > out[j].Opened })
	return out, nil
}

func (r *statusPageRuntime) Incident(id string) (core.StatusIncident, error) {
	if err := r.guard(); err != nil {
		return core.StatusIncident{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	i, ok := r.im.find(id)
	if !ok {
		return core.StatusIncident{}, fmt.Errorf("no such incident %q: %w", id, core.ErrNotFound)
	}
	return r.data.Incidents[i], nil
}

// mutate runs f under the lock and persists on success.
func (r *statusPageRuntime) mutate(f func() (core.StatusIncident, error)) (core.StatusIncident, error) {
	if err := r.guard(); err != nil {
		return core.StatusIncident{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	inc, err := f()
	if err != nil {
		return inc, err
	}
	return inc, r.persistLocked()
}

func (r *statusPageRuntime) CreateIncident(in core.NewIncident, actor string) (core.StatusIncident, error) {
	return r.mutate(func() (core.StatusIncident, error) { return r.im.create(in, actor) })
}

func (r *statusPageRuntime) PostUpdate(id string, u core.NewUpdate, actor string) (core.StatusIncident, error) {
	return r.mutate(func() (core.StatusIncident, error) { return r.im.post(id, u, actor) })
}

func (r *statusPageRuntime) EditUpdate(id, updateID string, u core.NewUpdate, actor string) (core.StatusIncident, error) {
	return r.mutate(func() (core.StatusIncident, error) { return r.im.editUpdate(id, updateID, u, actor) })
}

func (r *statusPageRuntime) EditIncident(id, title string, services []string, actor string) (core.StatusIncident, error) {
	return r.mutate(func() (core.StatusIncident, error) { return r.im.editIncident(id, title, services, actor) })
}

func (r *statusPageRuntime) DeleteIncident(id, actor string) error {
	_, err := r.mutate(func() (core.StatusIncident, error) { return core.StatusIncident{}, r.im.delete(id) })
	return err
}

func (r *statusPageRuntime) Public() (core.PublicStatus, error) {
	if err := r.guard(); err != nil {
		return core.PublicStatus{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if !r.pubAt.IsZero() && now.Sub(r.pubAt) < publicCacheTTL {
		return r.pubVal, nil
	}
	r.pubVal, r.pubAt = buildPublicStatus(r.getCfg().StatusTitle(), r.data, now), now
	return r.pubVal, nil
}

// buildPublicStatus is the ONLY producer of anonymous output. It copies
// named, public fields explicitly; never add a struct copy here.
func buildPublicStatus(title string, d *statusPageData, now time.Time) core.PublicStatus {
	names := map[string]string{}
	svcs := slices.Clone(d.Services)
	sort.SliceStable(svcs, func(i, j int) bool { return svcs[i].Order < svcs[j].Order })
	out := core.PublicStatus{Schema: 1, Title: title, Generated: now.Unix(), Overall: core.OverallOperational,
		Services: []core.PublicService{}, Active: []core.PublicIncident{}, Recent: []core.PublicIncident{}}
	worst := core.ServiceState("")
	for _, s := range svcs {
		names[s.ID] = s.Name
		state := core.StateOperational
		var hist map[string]core.ServiceState
		if rs := d.State[s.ID]; rs != nil {
			if rs.State != "" {
				state = rs.State
			}
			hist = rs.History
		}
		worst = core.WorseState(worst, state)
		days := make([]core.PublicDay, statusHistoryDays)
		for i := range days {
			day := dayKey(now.AddDate(0, 0, -(statusHistoryDays - 1 - i)))
			days[i] = core.PublicDay{Date: day, State: hist[day]}
		}
		out.Services = append(out.Services, core.PublicService{Name: s.Name, Description: s.Description, Group: s.Group, State: state, History: days})
	}
	switch worst {
	case core.StateOutage:
		out.Overall = core.OverallMajor
	case core.StateDegraded:
		out.Overall = core.OverallPartial
	case core.StateMaintenance:
		out.Overall = core.OverallMaintenance
	}
	recentCut := now.AddDate(0, 0, -statusHistoryDays).Unix()
	incs := slices.Clone(d.Incidents)
	sort.SliceStable(incs, func(i, j int) bool { return incs[i].Opened > incs[j].Opened })
	for _, inc := range incs {
		pi := core.PublicIncident{ID: inc.ID, Title: inc.Title, Impact: inc.Impact, Status: inc.Status,
			Opened: inc.Opened, Updated: inc.Updated, Resolved: inc.Resolved, Services: []string{}, Updates: []core.PublicUpdate{}}
		for _, id := range inc.Services {
			if n, ok := names[id]; ok {
				pi.Services = append(pi.Services, n)
			}
		}
		for i := len(inc.Updates) - 1; i >= 0; i-- {
			u := inc.Updates[i]
			pi.Updates = append(pi.Updates, core.PublicUpdate{TS: u.TS, Status: u.Status, Message: u.Message})
		}
		switch {
		case inc.Status != core.IncidentResolved:
			out.Active = append(out.Active, pi)
		case inc.Resolved >= recentCut:
			out.Recent = append(out.Recent, pi)
		}
	}
	return out
}
