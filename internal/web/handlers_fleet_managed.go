// handlers_fleet_managed.go: GET /fleet/managed -- the managed-config fragment list +
// create/edit form, and the per-node managed-config status table.
package web

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// managedKeyRow is one key/value row of the create/edit form's draft: Key is constrained
// client-side to core.ManagedKeys via a <select>.
type managedKeyRow struct {
	Key   string
	Value string
	Err   string
}

// managedDraft is the create/edit form's entire working copy.
type managedDraft struct {
	ID   string
	Tag  string
	Rows []managedKeyRow
}

// newManagedDraft is the fresh-GET default: no id, no tag, one blank row.
func newManagedDraft() managedDraft {
	return managedDraft{Rows: []managedKeyRow{{}}}
}

// managedDraftFromFragment loads an existing fragment into a draft for editing: one row per
// key ACTUALLY SET on the fragment, in core.ManagedKeys' fixed order.
func managedDraftFromFragment(f core.ManagedFragment) managedDraft {
	d := managedDraft{ID: f.ID, Tag: f.Tag}
	for _, k := range core.ManagedKeys {
		if v, ok := f.Values[k]; ok {
			d.Rows = append(d.Rows, managedKeyRow{Key: k, Value: v})
		}
	}
	if len(d.Rows) == 0 {
		d.Rows = []managedKeyRow{{}}
	}
	return d
}

// parseManagedDraftForm reads the posted draft off r: "id"/"tag" plus "row_count" indexed
// "row_<i>_key"/"row_<i>_value" rows.
func parseManagedDraftForm(r *http.Request) managedDraft {
	d := managedDraft{ID: r.FormValue("id"), Tag: r.FormValue("tag")}
	count, _ := strconv.Atoi(r.FormValue("row_count"))
	for i := 0; i < count; i++ {
		p := fmt.Sprintf("row_%d_", i)
		d.Rows = append(d.Rows, managedKeyRow{
			Key:   r.FormValue(p + "key"),
			Value: r.FormValue(p + "value"),
		})
	}
	if len(d.Rows) == 0 {
		d.Rows = []managedKeyRow{{}}
	}
	return d
}

// applyManagedRowOp reshapes rows in place for an "add_row"/"remove_row:<i>" op token --
// mirrors applyMatcherRowOp exactly, over this page's own row shape.
func applyManagedRowOp(rows *[]managedKeyRow, op string) {
	parts := strings.Split(op, ":")
	switch parts[0] {
	case "add_row":
		*rows = append(*rows, managedKeyRow{})
	case "remove_row":
		if i, ok := opIndexAt(parts, 1); ok && i < len(*rows) && len(*rows) > 1 {
			r := *rows
			*rows = append(r[:i], r[i+1:]...)
		}
	}
}

// managedRowsToValues converts rows to a Values map, DROPPING any row with a blank Key.
func managedRowsToValues(rows []managedKeyRow) map[string]string {
	out := map[string]string{}
	for _, row := range rows {
		if row.Key == "" {
			continue
		}
		out[row.Key] = row.Value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// managedUnknownKeyErrRe matches fleet_managed.go's validateManagedFragmentValues' exact
// "not a managed-config key" wording.
var managedUnknownKeyErrRe = regexp.MustCompile(`^"([^"]+)" is not a managed-config key`)

// managedErrKey extracts the ManagedFragment key a SaveManaged rejection names, if any:
// either the exact key in an "unknown key" rejection (managedUnknownKeyErrRe).
func managedErrKey(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if m := managedUnknownKeyErrRe.FindStringSubmatch(msg); m != nil {
		return m[1]
	}
	for _, k := range core.ManagedKeys {
		if strings.HasPrefix(msg, k+": ") {
			return k
		}
	}
	return ""
}

// applyManagedRowErr sets Err on every row of draft whose Key matches the key managedErrKey
// extracted from err, and reports whether any row was actually matched.
func applyManagedRowErr(rows []managedKeyRow, err error) (matched bool) {
	key := managedErrKey(err)
	if key == "" {
		return false
	}
	for i := range rows {
		if rows[i].Key == key {
			rows[i].Err = err.Error()
			matched = true
		}
	}
	return matched
}

// managedKeyOptions is the key <select>'s fixed option list: core.ManagedKeys
// verbatim, in that same closed order.
func managedKeyOptions() []string { return core.ManagedKeys }

// buildManagedRosterTags collects every DISTINCT tag currently carried by any node in the
// roster (read through r's request-scoped fleetMemo, exactly like buildSilenceNodeNames).
func buildManagedRosterTags(r *http.Request, d Deps) []string {
	nodes, err := fleetMemoFrom(r).fleetNodes(d)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, n := range nodes {
		for _, t := range n.Tags {
			if !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	sort.Strings(out)
	return out
}

// --------------------------------------------------------------------------- Fragments
// list ---------------------------------------------------------------------------

// ManagedFragmentRow is one row of the fragments table.
type ManagedFragmentRow struct {
	ID      string
	TagText string // "all nodes" when Tag is ""
	Tag     string // raw tag, "" for every node -- used by the Edit link/delete form
	KVText  string // "key=value, key=value" in core.ManagedKeys order
	Version int64
	Author  string
}

// describeManagedChange renders f's tag + keys/values as the audit log's New field.
func describeManagedChange(f core.ManagedFragment) string {
	row := newManagedFragmentRow(f)
	if row.KVText == "" {
		return "tag=" + row.TagText
	}
	return "tag=" + row.TagText + " " + row.KVText
}

func newManagedFragmentRow(f core.ManagedFragment) ManagedFragmentRow {
	tagText := f.Tag
	if tagText == "" {
		tagText = "all nodes"
	}
	var parts []string
	for _, k := range core.ManagedKeys {
		if v, ok := f.Values[k]; ok {
			parts = append(parts, k+"="+v)
		}
	}
	return ManagedFragmentRow{
		ID: f.ID, TagText: tagText, Tag: f.Tag,
		KVText: strings.Join(parts, ", "), Version: f.Version, Author: f.Author,
	}
}

// --------------------------------------------------------------------------- Per-node
// status ---------------------------------------------------------------------------

// ManagedStatusRow is one row of the per-node status table.
type ManagedStatusRow struct {
	Node        string
	NodeID      string
	VersionText string // "<applied> / <desired>"
	AppliedText string // "yes" / "no"
	Error       string
	DriftKeys   []string
	Conflicts   []string // "key (frag1, frag2)" -- last fragment listed is the one that won
}

// newManagedStatusRow projects s into its row shape. nodeNames resolves s.Node (a raw
// registry id) to its roster display name.
func newManagedStatusRow(s core.ManagedStatus, nodeNames map[string]string) ManagedStatusRow {
	applied := "no"
	if s.Applied {
		applied = "yes"
	}
	conflicts := make([]string, 0, len(s.Conflicts))
	for _, c := range s.Conflicts {
		conflicts = append(conflicts, c.Key+" ("+strings.Join(c.Fragments, ", ")+")")
	}
	name := nodeNames[s.Node]
	if name == "" {
		name = s.Node
	}
	return ManagedStatusRow{
		Node:        name,
		NodeID:      s.Node,
		VersionText: fmt.Sprintf("%d / %d", s.Version, s.Desired),
		AppliedText: applied,
		Error:       s.Error,
		DriftKeys:   s.Drift,
		Conflicts:   conflicts,
	}
}

// --------------------------------------------------------------------------- Flash: a
// FIXED set of codes only (like resolveIncidentFlash).

func resolveManagedFlash(r *http.Request) (text string, isErr bool) {
	switch r.URL.Query().Get("flash") {
	case "fragment_saved":
		return "Fragment saved", false
	case "fragment_deleted":
		return "Fragment deleted", false
	}
	return "", false
}

// --------------------------------------------------------------------------- Page data
// ---------------------------------------------------------------------------

// ManagedPageData is what templates/fleet_managed.html's "content" block renders against.
type ManagedPageData struct {
	PageData

	Fragments []ManagedFragmentRow
	Statuses  []ManagedStatusRow

	Draft      managedDraft
	KeyOptions []string
	RosterTags []string
	FormErr    string // top-level error not attributed to any one row
	FormErrTop bool   // true when FormErr should render at the top of the form

	Flash    string
	FlashErr bool
}

// managedPageOptions is buildManagedPageData's input: transient,
// this-response-only state a fresh GET never carries.
type managedPageOptions struct {
	DraftLoaded bool
	Draft       managedDraft
	FormErr     string
	FormErrTop  bool

	Flash    string
	FlashErr bool
}

func buildManagedPageData(r *http.Request, d Deps, opts managedPageOptions) ManagedPageData {
	var frags []core.ManagedFragment
	var statuses []core.ManagedStatus
	if fleet, err := fleetAPIFor(d); err == nil {
		frags, _ = fleet.Managed()
		statuses, _ = fleet.ManagedStatus()
	}
	sort.SliceStable(frags, func(i, j int) bool { return frags[i].Tag < frags[j].Tag })
	rows := make([]ManagedFragmentRow, 0, len(frags))
	for _, f := range frags {
		rows = append(rows, newManagedFragmentRow(f))
	}
	sort.SliceStable(statuses, func(i, j int) bool { return statuses[i].Node < statuses[j].Node })
	nodeNames := fleetNodeNameLookup(r, d)
	srows := make([]ManagedStatusRow, 0, len(statuses))
	for _, s := range statuses {
		srows = append(srows, newManagedStatusRow(s, nodeNames))
	}

	draft := opts.Draft
	if !opts.DraftLoaded {
		draft = loadManagedDraftFromQuery(r, frags)
	}

	flash, flashErr := opts.Flash, opts.FlashErr
	if flash == "" {
		flash, flashErr = resolveManagedFlash(r)
	}

	return ManagedPageData{
		PageData:   newPageData(r, d, "Managed config", "Master-pushed config fragments and per-node status"),
		Fragments:  rows,
		Statuses:   srows,
		Draft:      draft,
		KeyOptions: managedKeyOptions(),
		RosterTags: buildManagedRosterTags(r, d),
		FormErr:    opts.FormErr,
		FormErrTop: opts.FormErrTop,
		Flash:      flash,
		FlashErr:   flashErr,
	}
}

// loadManagedDraftFromQuery backs a fresh GET /fleet/managed?edit=<id>: when edit names a
// fragment that actually exists, the draft is loaded from it (managedDraftFromFragment).
func loadManagedDraftFromQuery(r *http.Request, frags []core.ManagedFragment) managedDraft {
	id := r.URL.Query().Get("edit")
	if id == "" {
		return newManagedDraft()
	}
	for _, f := range frags {
		if f.ID == id {
			return managedDraftFromFragment(f)
		}
	}
	return newManagedDraft()
}

// --------------------------------------------------------------------------- Rendering
// ---------------------------------------------------------------------------

func renderManagedPage(w http.ResponseWriter, data ManagedPageData, status int) error {
	tmpl, err := template.New("base.html").Funcs(funcMap).
		ParseFS(templatesFS, "templates/base.html", "templates/fleet_managed.html")
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	return tmpl.ExecuteTemplate(w, "base.html", data)
}

// renderManagedError re-renders the full page with a top-level flash (FlashErr=true) at the
// given 4xx status, for an error that names no specific field/row.
func renderManagedError(w http.ResponseWriter, r *http.Request, d Deps, msg string, status int) {
	data := buildManagedPageData(r, d, managedPageOptions{Flash: msg, FlashErr: true})
	if err := renderManagedPage(w, data, status); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// redirectToManaged redirects to GET /fleet/managed with a fixed ?flash=
// code -- never anything the client posted (see this file's own top doc).
func redirectToManaged(w http.ResponseWriter, r *http.Request, flashCode string) {
	v := url.Values{}
	if flashCode != "" {
		v.Set("flash", flashCode)
	}
	target := "/fleet/managed"
	if enc := v.Encode(); enc != "" {
		target += "?" + enc
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// --------------------------------------------------------------------------- Handlers
// ---------------------------------------------------------------------------

// fleetManagedPageHandler serves GET /fleet/managed: viewer+.
func fleetManagedPageHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fleetGateHTML(w, r, d) {
			return
		}
		data := buildManagedPageData(r, d, managedPageOptions{})
		if err := renderManagedPage(w, data, http.StatusOK); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// fleetManagedSaveHandler serves POST /fleet/managed (admin+CSRF, fleetAdminMutation).
func fleetManagedSaveHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fleetGateHTML(w, r, d) {
			return
		}
		if err := r.ParseForm(); err != nil {
			renderManagedError(w, r, d, "invalid form", http.StatusBadRequest)
			return
		}
		fleet, err := fleetAPIFor(d)
		if err != nil {
			renderManagedError(w, r, d, "fleet not available", http.StatusNotFound)
			return
		}
		draft := parseManagedDraftForm(r)
		if op := r.FormValue("op"); op != "" && op != "save" {
			applyManagedRowOp(&draft.Rows, op)
			data := buildManagedPageData(r, d, managedPageOptions{Draft: draft, DraftLoaded: true})
			if rerr := renderManagedPage(w, data, http.StatusOK); rerr != nil {
				http.Error(w, rerr.Error(), http.StatusInternalServerError)
			}
			return
		}

		actor := auditUser(r)
		saved, serr := fleet.SaveManaged(core.ManagedFragment{
			ID:     draft.ID,
			Tag:    strings.TrimSpace(draft.Tag),
			Values: managedRowsToValues(draft.Rows),
		}, actor)
		if serr != nil {
			matched := applyManagedRowErr(draft.Rows, serr)
			data := buildManagedPageData(r, d, managedPageOptions{
				Draft: draft, DraftLoaded: true,
				FormErr: serr.Error(), FormErrTop: !matched,
			})
			if rerr := renderManagedPage(w, data, fleetAPIErrStatus(serr)); rerr != nil {
				http.Error(w, rerr.Error(), http.StatusInternalServerError)
			}
			return
		}
		logAudit(d, r, "fleet.managed.save", saved.ID, "", describeManagedChange(saved))
		redirectToManaged(w, r, "fragment_saved")
	}
}

// fleetManagedDeleteHandler serves POST /fleet/managed/{id}/delete (admin+CSRF,
// fleetAdminMutation), used with the in-page two-step confirm.
func fleetManagedDeleteHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fleetGateHTML(w, r, d) {
			return
		}
		fleet, err := fleetAPIFor(d)
		if err != nil {
			renderManagedError(w, r, d, "fleet not available", http.StatusNotFound)
			return
		}
		id := r.PathValue("id")
		actor := auditUser(r)
		// Look up the fragment BEFORE deleting it so the audit record's New field can describe
		// what was actually removed (tag + keys), not just its opaque id.
		desc := id
		if frags, ferr := fleet.Managed(); ferr == nil {
			for _, f := range frags {
				if f.ID == id {
					desc = describeManagedChange(f)
					break
				}
			}
		}
		if err := fleet.DeleteManaged(id, actor); err != nil {
			renderManagedError(w, r, d, err.Error(), fleetAPIErrStatus(err))
			return
		}
		logAudit(d, r, "fleet.managed.delete", id, "", desc)
		redirectToManaged(w, r, "fragment_deleted")
	}
}
