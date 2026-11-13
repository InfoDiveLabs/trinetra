package main

import "testing"

func up() Result   { return Result{OK: true} }
func down() Result { return Result{Detail: "boom"} }
func cert(days float64, ok bool) Result {
	return Result{OK: ok, HasCert: true, CertDays: days}
}

func kinds(ops []Op) []string {
	var out []string
	for _, o := range ops {
		if o.Resolve {
			out = append(out, "resolve:"+o.Key)
		} else {
			out = append(out, o.Severity+":"+o.Key)
		}
	}
	return out
}

func eq(t *testing.T, got []Op, want ...string) {
	t.Helper()
	g := kinds(got)
	if len(g) != len(want) {
		t.Fatalf("ops = %v, want %v", g, want)
	}
	for i := range g {
		if g[i] != want[i] {
			t.Fatalf("ops = %v, want %v", g, want)
		}
	}
}

func TestDownAfterNRuns(t *testing.T) {
	r := NewRules()
	c := Check{Name: "web", Kind: KindHTTP}
	eq(t, r.Observe(c, down(), 3, 14))
	eq(t, r.Observe(c, down(), 3, 14))
	eq(t, r.Observe(c, down(), 3, 14), "critical:check.web.down")
	eq(t, r.Observe(c, down(), 3, 14))
	eq(t, r.Observe(c, up(), 3, 14), "resolve:check.web.down")
	eq(t, r.Observe(c, up(), 3, 14))
}

func TestDownCounterResetsOnSuccess(t *testing.T) {
	r := NewRules()
	c := Check{Name: "web", Kind: KindHTTP}
	r.Observe(c, down(), 3, 14)
	r.Observe(c, down(), 3, 14)
	r.Observe(c, up(), 3, 14)
	eq(t, r.Observe(c, down(), 3, 14))
	eq(t, r.Observe(c, down(), 3, 14))
	eq(t, r.Observe(c, down(), 3, 14), "critical:check.web.down")
}

func TestDownThresholdLoweredRaisesWithoutNewFailure(t *testing.T) {
	r := NewRules()
	c := Check{Name: "web", Kind: KindTCP}
	r.Observe(c, down(), 5, 14)
	r.Observe(c, down(), 5, 14)
	eq(t, r.Observe(c, down(), 2, 14), "critical:check.web.down")
}

func TestCertExpiringRaisesAndResolves(t *testing.T) {
	r := NewRules()
	c := Check{Name: "site", Kind: KindTLS}
	eq(t, r.Observe(c, cert(40, true), 3, 14))
	eq(t, r.Observe(c, cert(13.5, true), 3, 14), "warning:cert.site.expiring")
	eq(t, r.Observe(c, cert(12, true), 3, 14))
	eq(t, r.Observe(c, cert(89, true), 3, 14), "resolve:cert.site.expiring")
	eq(t, r.Observe(c, cert(89, true), 3, 14))
}

func TestCertAlertSurvivesFailedProbeWithoutCert(t *testing.T) {
	r := NewRules()
	c := Check{Name: "site", Kind: KindTLS}
	eq(t, r.Observe(c, cert(5, true), 1, 14), "warning:cert.site.expiring")
	eq(t, r.Observe(c, down(), 3, 14))
}

func TestExpiredCertRaisesCertAndDown(t *testing.T) {
	r := NewRules()
	c := Check{Name: "site", Kind: KindTLS}
	eq(t, r.Observe(c, cert(-1, false), 1, 14), "critical:check.site.down", "warning:cert.site.expiring")
}

func TestForgetResolvesActive(t *testing.T) {
	r := NewRules()
	c := Check{Name: "web", Kind: KindTLS}
	r.Observe(c, cert(1, false), 1, 14)
	eq(t, r.Forget("web"), "resolve:check.web.down", "resolve:cert.web.expiring")
	eq(t, r.Forget("web"))
}

func TestRevertRetriesRaise(t *testing.T) {
	r := NewRules()
	c := Check{Name: "web", Kind: KindTCP}
	ops := r.Observe(c, down(), 1, 14)
	r.Revert(ops[0])
	eq(t, r.Observe(c, down(), 1, 14), "critical:check.web.down")
}
