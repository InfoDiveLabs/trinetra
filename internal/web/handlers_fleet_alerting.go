// handlers_fleet_alerting.go: the fleet alerting admin page -- GET/POST /fleet/alerting
// (routes/policies/rules editor, structured form or "edit as JSON").
package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// oneIndexed renders a zero-based loop index (route/policy/step) as its 1-based display
// label (U6, 2026-09-25 UI audit fix).
func oneIndexed(i int) int { return i + 1 }

// templateDict builds a map[string]any from alternating key/value arguments --
// fleet_alerting.html's own funcMap entry ("dict", templates.go).
func templateDict(pairs ...any) (map[string]any, error) {
	if len(pairs)%2 != 0 {
		return nil, fmt.Errorf("dict: odd number of arguments")
	}
	m := make(map[string]any, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		key, ok := pairs[i].(string)
		if !ok {
			return nil, fmt.Errorf("dict: key %v is not a string", pairs[i])
		}
		m[key] = pairs[i+1]
	}
	return m, nil
}

// --------------------------------------------------------------------------- Draft model:
// the structured form's row shapes, convertible both ways with core.AlertingConfig.

// alertingRowLabel is the display/match label for a row's name: "(unnamed)" for a blank
// one.
func alertingRowLabel(name string) string {
	if strings.TrimSpace(name) == "" {
		return "(unnamed)"
	}
	return name
}

// alertingFieldKey builds the field-path key a validation error's field name.
func alertingFieldKey(kind, name string) string {
	return kind + ":" + alertingRowLabel(name)
}

// AlertingMatcherRow is one OR'd matcher row within a route (core.Matcher's four fields).
type AlertingMatcherRow struct {
	Tag      string
	Node     string
	Rule     string
	Severity string
}

// AlertingRouteRow is one row of the routes editor.
type AlertingRouteRow struct {
	Name     string
	Matchers []AlertingMatcherRow
	Policy   string
	GroupBy  string // comma-joined core.Route.GroupBy
	Continue bool
	// FieldKey is this row's inline-error match key ("route:<name-or- (unnamed)>"), computed
	// alongside Name every time a row is built/ reshaped -- see alertingFieldKey.
	FieldKey string
}

// AlertingStepRow is one escalation step within a policy.
type AlertingStepRow struct {
	After    string
	Channels []string
}

// HasChannel reports whether name (a channel name, or "*") is selected on this step -- the
// channel-checkboxes template calls this to mark each checkbox's checked state.
func (s AlertingStepRow) HasChannel(name string) bool {
	for _, c := range s.Channels {
		if c == name {
			return true
		}
	}
	return false
}

// AlertingPolicyRow is one row of the policies editor.
type AlertingPolicyRow struct {
	Name         string
	Steps        []AlertingStepRow
	RepeatEvery  string
	SendResolved bool
	FieldKey     string
}

// AlertingRuleRow is one row of the aggregate-rules editor.
type AlertingRuleRow struct {
	Name     string
	Expr     string
	Severity string
	FieldKey string
}

// AlertingDraft is the structured editor's entire working copy: every row
// plus the loaded Version (SetAlerting's optimistic-concurrency token).
type AlertingDraft struct {
	Version       int64
	Routes        []AlertingRouteRow
	Policies      []AlertingPolicyRow
	DefaultPolicy string
	Rules         []AlertingRuleRow
}

// draftFromConfig projects a core.AlertingConfig (fresh from Alerting(), or
// echoed back after a save) into its editable row shape.
func draftFromConfig(cfg core.AlertingConfig) AlertingDraft {
	d := AlertingDraft{Version: cfg.Version, DefaultPolicy: cfg.DefaultPolicy}
	for _, r := range cfg.Routes {
		row := AlertingRouteRow{
			Name:     r.Name,
			Policy:   r.Policy,
			GroupBy:  strings.Join(r.GroupBy, ","),
			Continue: r.Continue,
			FieldKey: alertingFieldKey("route", r.Name),
		}
		for _, m := range r.Matchers {
			row.Matchers = append(row.Matchers, AlertingMatcherRow{Tag: m.Tag, Node: m.Node, Rule: m.Rule, Severity: m.Severity})
		}
		if len(row.Matchers) == 0 {
			row.Matchers = []AlertingMatcherRow{{}}
		}
		d.Routes = append(d.Routes, row)
	}
	for _, p := range cfg.Policies {
		row := AlertingPolicyRow{
			Name:         p.Name,
			RepeatEvery:  p.RepeatEvery,
			SendResolved: p.SendResolved == nil || *p.SendResolved,
			FieldKey:     alertingFieldKey("policy", p.Name),
		}
		for _, s := range p.Steps {
			row.Steps = append(row.Steps, AlertingStepRow{After: s.After, Channels: append([]string(nil), s.Channels...)})
		}
		if len(row.Steps) == 0 {
			row.Steps = []AlertingStepRow{{After: "0s"}}
		}
		d.Policies = append(d.Policies, row)
	}
	for _, ru := range cfg.Rules {
		d.Rules = append(d.Rules, AlertingRuleRow{Name: ru.Name, Expr: ru.Expr, Severity: ru.Severity, FieldKey: alertingFieldKey("rule", ru.Name)})
	}
	return d
}

// toConfig converts the draft back into a core.AlertingConfig for SetAlerting -- a blank
// matcher row (every field empty) is dropped rather than posted as a Matcher{}.
func (d AlertingDraft) toConfig() core.AlertingConfig {
	cfg := core.AlertingConfig{Version: d.Version, DefaultPolicy: strings.TrimSpace(d.DefaultPolicy)}
	for _, r := range d.Routes {
		var matchers []core.Matcher
		for _, m := range r.Matchers {
			if m.Tag == "" && m.Node == "" && m.Rule == "" && m.Severity == "" {
				continue
			}
			matchers = append(matchers, core.Matcher{Tag: m.Tag, Node: m.Node, Rule: m.Rule, Severity: m.Severity})
		}
		var groupBy []string
		for _, g := range strings.Split(r.GroupBy, ",") {
			g = strings.TrimSpace(g)
			if g != "" {
				groupBy = append(groupBy, g)
			}
		}
		cfg.Routes = append(cfg.Routes, core.Route{
			Name:     strings.TrimSpace(r.Name),
			Matchers: matchers,
			Policy:   strings.TrimSpace(r.Policy),
			GroupBy:  groupBy,
			Continue: r.Continue,
		})
	}
	for _, p := range d.Policies {
		var steps []core.PolicyStep
		for _, s := range p.Steps {
			steps = append(steps, core.PolicyStep{After: strings.TrimSpace(s.After), Channels: append([]string(nil), s.Channels...)})
		}
		sendResolved := p.SendResolved
		cfg.Policies = append(cfg.Policies, core.Policy{
			Name:         strings.TrimSpace(p.Name),
			Steps:        steps,
			RepeatEvery:  strings.TrimSpace(p.RepeatEvery),
			SendResolved: &sendResolved,
		})
	}
	for _, ru := range d.Rules {
		cfg.Rules = append(cfg.Rules, core.AggregateRule{Name: strings.TrimSpace(ru.Name), Expr: strings.TrimSpace(ru.Expr), Severity: ru.Severity})
	}
	return cfg
}

// alertingConfigJSON pretty-prints cfg for the "edit as JSON" textarea --
// "" (never a template error) if it somehow fails to marshal.
func alertingConfigJSON(cfg core.AlertingConfig) string {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}

// --------------------------------------------------------------------------- Form parsing:
// the structured editor posts every row as indexed fields.

func parseAlertingDraftForm(r *http.Request) AlertingDraft {
	var d AlertingDraft
	d.Version, _ = strconv.ParseInt(r.FormValue("version"), 10, 64)
	d.DefaultPolicy = r.FormValue("default_policy")

	routeCount, _ := strconv.Atoi(r.FormValue("route_count"))
	for i := 0; i < routeCount; i++ {
		p := fmt.Sprintf("route_%d_", i)
		row := AlertingRouteRow{
			Name:     r.FormValue(p + "name"),
			Policy:   r.FormValue(p + "policy"),
			GroupBy:  r.FormValue(p + "group_by"),
			Continue: r.FormValue(p+"continue") != "",
		}
		row.FieldKey = alertingFieldKey("route", row.Name)
		matcherCount, _ := strconv.Atoi(r.FormValue(p + "matcher_count"))
		for j := 0; j < matcherCount; j++ {
			mp := fmt.Sprintf("%smatcher_%d_", p, j)
			row.Matchers = append(row.Matchers, AlertingMatcherRow{
				Tag:      r.FormValue(mp + "tag"),
				Node:     r.FormValue(mp + "node"),
				Rule:     r.FormValue(mp + "rule"),
				Severity: r.FormValue(mp + "severity"),
			})
		}
		if len(row.Matchers) == 0 {
			row.Matchers = []AlertingMatcherRow{{}}
		}
		d.Routes = append(d.Routes, row)
	}

	policyCount, _ := strconv.Atoi(r.FormValue("policy_count"))
	for i := 0; i < policyCount; i++ {
		p := fmt.Sprintf("policy_%d_", i)
		row := AlertingPolicyRow{
			Name:         r.FormValue(p + "name"),
			RepeatEvery:  r.FormValue(p + "repeat_every"),
			SendResolved: r.FormValue(p+"send_resolved") != "",
		}
		row.FieldKey = alertingFieldKey("policy", row.Name)
		stepCount, _ := strconv.Atoi(r.FormValue(p + "step_count"))
		for j := 0; j < stepCount; j++ {
			sp := fmt.Sprintf("%sstep_%d_", p, j)
			row.Steps = append(row.Steps, AlertingStepRow{
				After:    r.FormValue(sp + "after"),
				Channels: append([]string(nil), r.Form[sp+"channels"]...),
			})
		}
		if len(row.Steps) == 0 {
			row.Steps = []AlertingStepRow{{After: "0s"}}
		}
		d.Policies = append(d.Policies, row)
	}

	ruleCount, _ := strconv.Atoi(r.FormValue("rule_count"))
	for i := 0; i < ruleCount; i++ {
		p := fmt.Sprintf("rule_%d_", i)
		row := AlertingRuleRow{
			Name:     r.FormValue(p + "name"),
			Expr:     r.FormValue(p + "expr"),
			Severity: r.FormValue(p + "severity"),
		}
		row.FieldKey = alertingFieldKey("rule", row.Name)
		d.Rules = append(d.Rules, row)
	}
	return d
}

// opIndexAt parses parts[i] as a non-negative int, ok=false if i is out of range or the
// value doesn't parse.
func opIndexAt(parts []string, i int) (int, bool) {
	if i >= len(parts) {
		return 0, false
	}
	n, err := strconv.Atoi(parts[i])
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// applyAlertingOp reshapes d in place for every add/remove/reorder op the structured
// editor's row buttons post.
func applyAlertingOp(d *AlertingDraft, op string) {
	parts := strings.Split(op, ":")
	switch parts[0] {
	case "add_route":
		d.Routes = append(d.Routes, AlertingRouteRow{Matchers: []AlertingMatcherRow{{}}, FieldKey: alertingFieldKey("route", "")})
	case "remove_route":
		if i, ok := opIndexAt(parts, 1); ok && i < len(d.Routes) {
			d.Routes = append(d.Routes[:i], d.Routes[i+1:]...)
		}
	case "move_route_up":
		if i, ok := opIndexAt(parts, 1); ok && i > 0 && i < len(d.Routes) {
			d.Routes[i-1], d.Routes[i] = d.Routes[i], d.Routes[i-1]
		}
	case "move_route_down":
		if i, ok := opIndexAt(parts, 1); ok && i >= 0 && i < len(d.Routes)-1 {
			d.Routes[i+1], d.Routes[i] = d.Routes[i], d.Routes[i+1]
		}
	case "add_matcher":
		if i, ok := opIndexAt(parts, 1); ok && i < len(d.Routes) {
			d.Routes[i].Matchers = append(d.Routes[i].Matchers, AlertingMatcherRow{})
		}
	case "remove_matcher":
		if i, ok := opIndexAt(parts, 1); ok && i < len(d.Routes) {
			if j, ok2 := opIndexAt(parts, 2); ok2 && j < len(d.Routes[i].Matchers) && len(d.Routes[i].Matchers) > 1 {
				m := d.Routes[i].Matchers
				d.Routes[i].Matchers = append(m[:j], m[j+1:]...)
			}
		}
	case "add_policy":
		d.Policies = append(d.Policies, AlertingPolicyRow{Steps: []AlertingStepRow{{After: "0s"}}, SendResolved: true, FieldKey: alertingFieldKey("policy", "")})
	case "remove_policy":
		if i, ok := opIndexAt(parts, 1); ok && i < len(d.Policies) {
			d.Policies = append(d.Policies[:i], d.Policies[i+1:]...)
		}
	case "add_step":
		if i, ok := opIndexAt(parts, 1); ok && i < len(d.Policies) {
			d.Policies[i].Steps = append(d.Policies[i].Steps, AlertingStepRow{After: "0s"})
		}
	case "remove_step":
		if i, ok := opIndexAt(parts, 1); ok && i < len(d.Policies) {
			if j, ok2 := opIndexAt(parts, 2); ok2 && j < len(d.Policies[i].Steps) && len(d.Policies[i].Steps) > 1 {
				s := d.Policies[i].Steps
				d.Policies[i].Steps = append(s[:j], s[j+1:]...)
			}
		}
	case "add_rule":
		d.Rules = append(d.Rules, AlertingRuleRow{Severity: "warning", FieldKey: alertingFieldKey("rule", "")})
	case "remove_rule":
		if i, ok := opIndexAt(parts, 1); ok && i < len(d.Rules) {
			d.Rules = append(d.Rules[:i], d.Rules[i+1:]...)
		}
	}
}

// --------------------------------------------------------------------------- Field-path
// error mapping.

var alertingErrPatterns = []struct {
	re    *regexp.Regexp
	field func(m []string) string
}{
	{regexp.MustCompile(`^policy "([^"]*)" step (\d+):`), func(m []string) string { return "policy:" + alertingRowLabel(m[1]) + ":step:" + m[2] }},
	{regexp.MustCompile(`^policy "([^"]*)":`), func(m []string) string { return "policy:" + alertingRowLabel(m[1]) }},
	{regexp.MustCompile(`^duplicate policy name "([^"]*)"$`), func(m []string) string { return "policy:" + alertingRowLabel(m[1]) }},
	{regexp.MustCompile(`^route "([^"]*)":`), func(m []string) string { return "route:" + alertingRowLabel(m[1]) }},
	{regexp.MustCompile(`^duplicate route name "([^"]*)"$`), func(m []string) string { return "route:" + alertingRowLabel(m[1]) }},
	{regexp.MustCompile(`^default_policy:`), func(m []string) string { return "default_policy" }},
	{regexp.MustCompile(`^rule "([^"]*)":`), func(m []string) string { return "rule:" + alertingRowLabel(m[1]) }},
	{regexp.MustCompile(`^duplicate rule name "([^"]*)"$`), func(m []string) string { return "rule:" + alertingRowLabel(m[1]) }},
}

func alertingErrField(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	for _, p := range alertingErrPatterns {
		if m := p.re.FindStringSubmatch(msg); m != nil {
			return p.field(m)
		}
	}
	return ""
}

// --------------------------------------------------------------------------- Page data
// ---------------------------------------------------------------------------

// RuleStateRow is one row of the /fleet/rules/state fragment (core.RuleState projected for
// display).
type RuleStateRow struct {
	Name      string
	Expr      string
	ValueText string
	StateText string
	SinceText string
	Error     string
}

func buildRuleStateRows(d Deps) []RuleStateRow {
	fleet, err := fleetAPIFor(d)
	if err != nil {
		return nil
	}
	states, err := fleet.RuleStates()
	if err != nil {
		return nil
	}
	rows := make([]RuleStateRow, 0, len(states))
	for _, s := range states {
		row := RuleStateRow{Name: s.Name, Expr: s.Expr, Error: s.Error}
		if s.HasValue && !s.NoData {
			row.ValueText = strconv.FormatFloat(s.Value, 'f', -1, 64)
		} else {
			row.ValueText = "no data"
		}
		switch {
		case s.Error != "":
			row.StateText = "error"
		case s.Firing:
			row.StateText = "firing"
		default:
			row.StateText = "ok"
		}
		if s.Since > 0 {
			row.SinceText = nodeAgoText(s.Since)
		} else {
			row.SinceText = "-"
		}
		rows = append(rows, row)
	}
	return rows
}

// AlertingTestStep is one policy step of a route-test result, rendered
// After -> Channels (comma-joined).
type AlertingTestStep struct {
	After    string
	Channels string
}

// AlertingTestPolicy is one matched policy of a route-test result -- core.RouteDecision
// carries several when a Continue chain fanned out (core.RouteDecision's own doc).
type AlertingTestPolicy struct {
	Name         string
	Steps        []AlertingTestStep
	RepeatEvery  string
	SendResolved bool
}

// AlertingTestResult is core.RouteDecision projected for display.
type AlertingTestResult struct {
	Route      string
	Policies   []AlertingTestPolicy
	Suppressed string
}

func buildAlertingTestResult(dec core.RouteDecision) *AlertingTestResult {
	res := &AlertingTestResult{Route: dec.Route, Suppressed: dec.Suppressed}
	for _, p := range dec.Policies {
		tp := AlertingTestPolicy{Name: p.Name, RepeatEvery: p.RepeatEvery, SendResolved: p.SendResolved == nil || *p.SendResolved}
		for _, s := range p.Steps {
			tp.Steps = append(tp.Steps, AlertingTestStep{After: s.After, Channels: strings.Join(s.Channels, ", ")})
		}
		res.Policies = append(res.Policies, tp)
	}
	return res
}

// AlertingPageData is what templates/fleet_alerting.html's "content" block renders against.
type AlertingPageData struct {
	PageData

	Draft      AlertingDraft
	JSONConfig string
	// JSONErr is a "edit as JSON" mode-specific error.
	JSONErr string

	// Channels are the master's configured channel names (d.Cfg().Channels), offered alongside
	// the literal "*" in every step's channel checkboxes.
	Channels    []string
	PolicyNames []string

	Flash    string
	FlashErr bool
	// ErrField names the one row/step a validation error targets
	// (alertingErrField), "" when it names none (renders at the top only).
	ErrField string

	Rules []RuleStateRow

	TestNodes    []string
	TestNode     string
	TestRule     string
	TestSeverity string
	TestResult   *AlertingTestResult
	TestErr      string
}

// alertingPageOptions is buildAlertingPageData's input: Loaded distinguishes a fresh GET
// (load Alerting() fresh) from a POST re-render.
type alertingPageOptions struct {
	Loaded     bool
	Draft      AlertingDraft
	JSONConfig string

	Flash    string
	FlashErr bool
	ErrField string

	TestNode     string
	TestRule     string
	TestSeverity string
	TestResult   *AlertingTestResult
	TestErr      string
}

func buildAlertingChannelNames(d Deps) []string {
	var names []string
	if d.Cfg != nil {
		if cfg := d.Cfg(); cfg != nil {
			for _, c := range cfg.Channels {
				names = append(names, c.Name)
			}
		}
	}
	sort.Strings(names)
	return names
}

func buildAlertingPolicyNames(draft AlertingDraft) []string {
	names := make([]string, 0, len(draft.Policies))
	for _, p := range draft.Policies {
		if p.Name != "" {
			names = append(names, p.Name)
		}
	}
	return names
}

// buildAlertingTestNodeNames reads the roster through r's request-scoped fleetMemo
// (fleet_memo.go) rather than a fresh Fleet().Nodes() call.
func buildAlertingTestNodeNames(r *http.Request, d Deps) []string {
	nodes, err := fleetMemoFrom(r).fleetNodes(d)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(nodes))
	for _, n := range nodes {
		names = append(names, n.Name)
	}
	sort.Strings(names)
	return names
}

func buildAlertingPageData(r *http.Request, d Deps, opts alertingPageOptions) AlertingPageData {
	var draft AlertingDraft
	var jsonConfig string
	if opts.Loaded {
		draft = opts.Draft
		jsonConfig = opts.JSONConfig
	} else {
		var cfg core.AlertingConfig
		if fleet, err := fleetAPIFor(d); err == nil {
			cfg, _ = fleet.Alerting()
		}
		draft = draftFromConfig(cfg)
		jsonConfig = alertingConfigJSON(cfg)
	}

	return AlertingPageData{
		PageData:     newPageData(r, d, "Alerting", "Routes, policies, rules, and the route tester"),
		Draft:        draft,
		JSONConfig:   jsonConfig,
		Channels:     buildAlertingChannelNames(d),
		PolicyNames:  buildAlertingPolicyNames(draft),
		Flash:        opts.Flash,
		FlashErr:     opts.FlashErr,
		ErrField:     opts.ErrField,
		Rules:        buildRuleStateRows(d),
		TestNodes:    buildAlertingTestNodeNames(r, d),
		TestNode:     opts.TestNode,
		TestRule:     opts.TestRule,
		TestSeverity: opts.TestSeverity,
		TestResult:   opts.TestResult,
		TestErr:      opts.TestErr,
	}
}

// --------------------------------------------------------------------------- Rendering
// ---------------------------------------------------------------------------

func renderAlertingPage(w http.ResponseWriter, data AlertingPageData, status int) error {
	tmpl, err := template.New("base.html").Funcs(funcMap).
		ParseFS(templatesFS, "templates/base.html", "templates/fleet_alerting.html")
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	return tmpl.ExecuteTemplate(w, "base.html", data)
}

func renderAlertingError(w http.ResponseWriter, r *http.Request, d Deps, msg string, status int) {
	data := buildAlertingPageData(r, d, alertingPageOptions{Flash: msg, FlashErr: true})
	if err := renderAlertingPage(w, data, status); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// renderRuleStatesFragment renders fleet_alerting.html's "rule_rows" block
// alone -- GET /fleet/rules/state's htmx poll target.
func renderRuleStatesFragment(w http.ResponseWriter, rows []RuleStateRow) error {
	tmpl, err := template.New("fleet_alerting.html").Funcs(funcMap).
		ParseFS(templatesFS, "templates/fleet_alerting.html")
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	return tmpl.ExecuteTemplate(w, "rule_rows", struct{ Rows []RuleStateRow }{rows})
}

// AlertingTestResultData is what fleet_alerting.html's "test_result" block renders against.
type AlertingTestResultData struct {
	TestResult *AlertingTestResult
	TestErr    string
}

func renderAlertingTestFragment(w http.ResponseWriter, data AlertingTestResultData, status int) error {
	tmpl, err := template.New("fleet_alerting.html").Funcs(funcMap).
		ParseFS(templatesFS, "templates/fleet_alerting.html")
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	return tmpl.ExecuteTemplate(w, "test_result", data)
}

// --------------------------------------------------------------------------- Handlers
// ---------------------------------------------------------------------------

// fleetAlertingPageHandler serves GET /fleet/alerting: read-only for a
// viewer, the routes/policies/rules editor (+ "edit as JSON") for an admin.
func fleetAlertingPageHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fleetGateHTML(w, r, d) {
			return
		}
		data := buildAlertingPageData(r, d, alertingPageOptions{})
		if err := renderAlertingPage(w, data, http.StatusOK); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// alertingConflictMessage is the wording for a stale-Version SetAlerting
// rejection (core.ErrConflict).
const alertingConflictMessage = "the alerting config changed since you loaded it — reload to see the latest"

// alertingMaxBodyBytes caps POST /fleet/alerting's request body.
const alertingMaxBodyBytes = 256 * 1024

// decodeAlertingJSON decodes raw as exactly one core.AlertingConfig JSON value:
// DisallowUnknownFields rejects an unknown field by name (e.g. a typo'd "send_resolve").
func decodeAlertingJSON(raw string) (core.AlertingConfig, error) {
	var cfg core.AlertingConfig
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return core.AlertingConfig{}, err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return core.AlertingConfig{}, errors.New("must contain exactly one JSON value (no trailing data)")
	}
	return cfg, nil
}

// fleetAlertingSaveHandler serves POST /fleet/alerting (admin+CSRF, fleetAdminMutation):
// mode=json submits the "edit as JSON" textarea verbatim as a full AlertingConfig.
func limitBody(n int64, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > n {
			http.Error(w, "request too large (max 256 KiB)", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, n)
		next(w, r)
	}
}

func fleetAlertingSaveHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fleetGateHTML(w, r, d) {
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, alertingMaxBodyBytes)
		if err := r.ParseForm(); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				renderAlertingError(w, r, d, "request too large (max 256 KiB)", http.StatusRequestEntityTooLarge)
				return
			}
			renderAlertingError(w, r, d, "invalid form", http.StatusBadRequest)
			return
		}
		fleet, err := fleetAPIFor(d)
		if err != nil {
			renderAlertingError(w, r, d, "fleet not available", http.StatusNotFound)
			return
		}
		actor := auditUser(r)

		if r.FormValue("mode") == "json" {
			raw := r.FormValue("json_config")
			cfg, jerr := decodeAlertingJSON(raw)
			if jerr != nil {
				msg := "invalid JSON: " + jerr.Error()
				// The structured panel reloads the untouched canonical config (nothing was saved) --
				// only the JSON textarea and its own inline error reflect what was actually posted.
				data := buildAlertingPageData(r, d, alertingPageOptions{})
				data.JSONConfig, data.JSONErr = raw, msg
				data.Flash, data.FlashErr = msg, true
				renderAlertingPage(w, data, http.StatusBadRequest)
				return
			}
			if serr := fleet.SetAlerting(cfg, actor); serr != nil {
				status, flash := alertingSaveErrorStatus(serr)
				data := buildAlertingPageData(r, d, alertingPageOptions{})
				data.JSONConfig, data.JSONErr = raw, flash
				data.Flash, data.FlashErr = flash, true
				data.ErrField = alertingErrField(serr)
				renderAlertingPage(w, data, status)
				return
			}
			logAudit(d, r, "fleet.alerting.set", "", "", "mode=json")
			renderAlertingPage(w, buildAlertingPageData(r, d, alertingPageOptions{Flash: "alerting config saved"}), http.StatusOK)
			return
		}

		draft := parseAlertingDraftForm(r)
		op := r.FormValue("op")
		if op != "" && op != "save" {
			applyAlertingOp(&draft, op)
			data := buildAlertingPageData(r, d, alertingPageOptions{Loaded: true, Draft: draft, JSONConfig: alertingConfigJSON(draft.toConfig())})
			renderAlertingPage(w, data, http.StatusOK)
			return
		}

		cfg := draft.toConfig()
		if serr := fleet.SetAlerting(cfg, actor); serr != nil {
			status, flash := alertingSaveErrorStatus(serr)
			data := buildAlertingPageData(r, d, alertingPageOptions{
				Loaded: true, Draft: draft, JSONConfig: alertingConfigJSON(cfg),
				Flash: flash, FlashErr: true, ErrField: alertingErrField(serr),
			})
			renderAlertingPage(w, data, status)
			return
		}
		logAudit(d, r, "fleet.alerting.set", "", "", "mode=form")
		renderAlertingPage(w, buildAlertingPageData(r, d, alertingPageOptions{Flash: "alerting config saved"}), http.StatusOK)
	}
}

// alertingSaveErrorStatus maps a SetAlerting error to its render status/flash text:
// core.ErrConflict is the 409 + fixed message; anything else is a validation rejection.
func alertingSaveErrorStatus(err error) (status int, flash string) {
	if errors.Is(err, core.ErrConflict) {
		return http.StatusConflict, alertingConflictMessage
	}
	return http.StatusBadRequest, err.Error()
}

// fleetAlertingTestHandler serves POST /fleet/alerting/test: the route tester.
func fleetAlertingTestHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fleetGatePlain(w, r, d) {
			return
		}
		if err := r.ParseForm(); err != nil {
			renderAlertingTestFragment(w, AlertingTestResultData{TestErr: "invalid form"}, http.StatusBadRequest)
			return
		}
		fleet, err := fleetAPIFor(d)
		if err != nil {
			renderAlertingTestFragment(w, AlertingTestResultData{TestErr: "fleet not available"}, http.StatusNotFound)
			return
		}
		alert := core.TestAlert{
			Node:     r.FormValue("node"),
			Rule:     r.FormValue("rule"),
			Severity: r.FormValue("severity"),
		}
		decision, terr := fleet.RouteTest(alert)
		if terr != nil {
			renderAlertingTestFragment(w, AlertingTestResultData{TestErr: terr.Error()}, fleetAPIErrStatus(terr))
			return
		}
		renderAlertingTestFragment(w, AlertingTestResultData{TestResult: buildAlertingTestResult(decision)}, http.StatusOK)
	}
}

// fleetRulesStateHandler serves GET /fleet/rules/state: the htmx poll target.
func fleetRulesStateHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fleetGatePlain(w, r, d) {
			return
		}
		if err := renderRuleStatesFragment(w, buildRuleStateRows(d)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}
