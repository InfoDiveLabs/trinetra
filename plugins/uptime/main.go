// Command trinetra-plugin-uptime is the Trinetra uptime plugin: HTTP, TCP, TLS-expiry and DNS checks.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
	"github.com/InfoDiveLabs/trinetra/pkg/pluginsdk"
)

const (
	version         = "0.1.0"
	maxChecks       = 200
	defaultInterval = 60 * time.Second
	defaultTimeout  = 10 * time.Second
	defaultDownRuns = 3
	defaultCertDays = 14
)

type app struct {
	p      *pluginsdk.Plugin
	prober *Prober
	store  *Store
	rules  *Rules
	sched  *Scheduler

	mu           sync.Mutex
	downAfter    int
	certWarnDays int
	warnedReject bool
}

func newApp(prober *Prober) *app {
	a := &app{
		p: pluginsdk.New(pluginsdk.Options{Version: version}), prober: prober,
		store: NewStore(), rules: NewRules(), downAfter: defaultDownRuns, certWarnDays: defaultCertDays,
	}
	a.sched = NewScheduler(prober.Run, a.onResult)
	a.p.OnConfig(a.apply)
	a.p.HandleValidateConfig(validateConfig)
	a.p.HandleSlot("checks", func(ctx context.Context, req plugin.SlotRequest) ([]plugin.Widget, error) {
		cs, res := a.store.Snapshot()
		return ChecksPage(cs, res), nil
	})
	a.p.HandleSlot("overview", func(ctx context.Context, req plugin.SlotRequest) ([]plugin.Widget, error) {
		cs, res := a.store.Snapshot()
		return Overview(cs, res), nil
	})
	return a
}

type parsed struct {
	checks []Check
	errs   []string
}

func parseLines(lines []string) parsed {
	var out parsed
	seen := map[string]int{}
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		c, err := ParseCheck(line)
		if err != nil {
			out.errs = append(out.errs, fmt.Sprintf("line %d %v", i+1, err))
			continue
		}
		if first, dup := seen[c.Name]; dup {
			out.errs = append(out.errs, fmt.Sprintf("line %d: name %q already used on line %d", i+1, c.Name, first))
			continue
		}
		seen[c.Name] = i + 1
		out.checks = append(out.checks, c)
	}
	if len(lines) > maxChecks {
		out.errs = append(out.errs, fmt.Sprintf("at most %d checks", maxChecks))
	}
	return out
}

func joinErrs(errs []string) string {
	const show = 3
	if len(errs) <= show {
		return strings.Join(errs, "; ")
	}
	return fmt.Sprintf("%s; and %d more", strings.Join(errs[:show], "; "), len(errs)-show)
}

func validateConfig(cfg pluginsdk.Config) plugin.FieldErrors {
	p := parseLines(cfg.Strings("checks"))
	if len(p.errs) == 0 {
		return nil
	}
	return plugin.FieldErrors{"checks": joinErrs(p.errs)}
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

func (a *app) apply(cfg pluginsdk.Config) {
	p := parseLines(cfg.Strings("checks"))
	if len(p.checks) > maxChecks {
		p.checks = p.checks[:maxChecks]
	}
	a.mu.Lock()
	a.downAfter = int(cfg.Int("down_after_runs"))
	if a.downAfter < 1 {
		a.downAfter = defaultDownRuns
	}
	a.certWarnDays = int(cfg.Int("cert_warn_days"))
	if a.certWarnDays < 1 {
		a.certWarnDays = defaultCertDays
	}
	a.mu.Unlock()

	a.sched.Stop()
	live := map[string]bool{}
	for _, c := range p.checks {
		live[c.Name] = true
	}
	for _, name := range a.rules.Names() {
		if !live[name] {
			a.send(a.rules.Forget(name))
		}
	}
	a.store.SetChecks(p.checks)
	a.sched.Apply(p.checks, orDefault(cfg.Duration("default_interval"), defaultInterval), orDefault(cfg.Duration("default_timeout"), defaultTimeout))

	switch {
	case len(p.errs) > 0:
		_ = a.p.ReportStatus("degraded", fmt.Sprintf("%d checks running; %s", len(p.checks), joinErrs(p.errs)))
	default:
		_ = a.p.ReportStatus("ok", fmt.Sprintf("%d checks running", len(p.checks)))
	}
}

func (a *app) onResult(c Check, r Result) {
	a.store.Record(c, r)
	m := map[string]float64{"check." + c.Name + ".up": 0}
	if r.OK {
		m["check."+c.Name+".up"] = 1
		m["check."+c.Name+".latency_ms"] = float64(r.Latency.Microseconds()) / 1000
	}
	if r.HasCert {
		m["cert."+c.Name+".days_left"] = r.CertDays
	}
	if resp, err := a.p.Publish(0, m); err != nil {
		a.p.Logf("warn", "publish %s: %v", c.Name, err)
	} else if len(resp.Rejected) > 0 {
		a.mu.Lock()
		first := !a.warnedReject
		a.warnedReject = true
		a.mu.Unlock()
		if first {
			a.p.Logf("warn", "host rejected series %s", strings.Join(resp.Rejected, ", "))
		}
	}
	a.mu.Lock()
	down, cert := a.downAfter, a.certWarnDays
	a.mu.Unlock()
	a.send(a.rules.Observe(c, r, down, cert))
}

func (a *app) send(ops []Op) {
	for _, op := range ops {
		var err error
		if op.Resolve {
			err = a.p.ResolveAlert(op.Key, op.Body)
		} else {
			err = a.p.RaiseAlert(op.Key, op.Severity, op.Title, op.Body)
		}
		if err != nil {
			a.rules.Revert(op)
			a.p.Logf("warn", "alert %s: %v", op.Key, err)
		}
	}
}

func (a *app) run(ctx context.Context) error {
	defer a.sched.Stop()
	return a.p.Run(ctx)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := newApp(&Prober{}).run(ctx); err != nil {
		log.Fatal(err)
	}
}
