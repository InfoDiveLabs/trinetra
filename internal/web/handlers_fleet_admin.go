package web

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// fleetTokenTTLMin/Max/Default are task-7-brief.md's exact TTL bounds for
// POST /fleet/tokens: a Go duration string, 5 minutes to 720 hours (30
// days), defaulting to 1 hour when the field is left blank.
const (
	fleetTokenTTLMin     = 5 * time.Minute
	fleetTokenTTLMax     = 720 * time.Hour
	fleetTokenTTLDefault = "1h"
)

// fleetTokenUsesMin/Max are the brief's uses bounds: 1..100, default 1.
const (
	fleetTokenUsesMin = 1
	fleetTokenUsesMax = 100
)

// fleetMaxTags is the brief's cap on how many tags a single tags= field may
// carry (token creation and the node tags form both use it).
const fleetMaxTags = 10

// fleetTagRe is the brief's exact tag shape: lowercase letters, digits,
// underscore, hyphen, 1-32 characters. Deliberately narrower than
// internal/fleet.ValidTag (which also allows '.') -- this is the web
// layer's OWN validation ahead of the daemon's, using the brief's literal
// values rather than reusing that package's regex (internal/web doesn't
// import internal/fleet).
var fleetTagRe = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// fleetNodeNameMax is the brief's rename bound: 1..64 characters.
const fleetNodeNameMax = 64

// fleetAdminMutation composes requireRole(RoleAdmin, ...) with requireCSRF,
// exactly like usersMutation (handlers_users.go): every /fleet/tokens* and
// /fleet/nodes/{id}/* mutation needs both gates. The "master only, else
// 404" half of the brief's "admin + CSRF for POST; master only else 404"
// ruling is enforced separately, inside each handler (fleetGateHTML) --
// requireRole/requireCSRF only ever gate on session/role/CSRF, never on
// this daemon's fleet role.
func fleetAdminMutation(d Deps, next http.HandlerFunc) http.HandlerFunc {
	return requireRole(RoleAdmin, d, func(w http.ResponseWriter, r *http.Request) {
		requireCSRF(next).ServeHTTP(w, r)
	})
}

// parseFleetTokenTTL validates POST /fleet/tokens' "ttl" field: a blank
// value normalizes to fleetTokenTTLDefault (no error); anything else must
// parse as a Go duration string within [fleetTokenTTLMin,fleetTokenTTLMax].
// normalized is what the create-token form should echo back (either the
// user's own input or the applied default), used both to build the
// TokenSpec (via the returned Duration) and to keep the form sticky on a
// later validation error elsewhere in the same submission.
func parseFleetTokenTTL(raw string) (d time.Duration, normalized string, err error) {
	normalized = strings.TrimSpace(raw)
	if normalized == "" {
		normalized = fleetTokenTTLDefault
	}
	d, err = time.ParseDuration(normalized)
	if err != nil {
		return 0, normalized, fmt.Errorf("ttl must be a Go duration like \"1h\" or \"30m\"")
	}
	if d < fleetTokenTTLMin || d > fleetTokenTTLMax {
		return 0, normalized, fmt.Errorf("ttl must be between 5m and 720h")
	}
	return d, normalized, nil
}

// parseFleetTokenUses validates POST /fleet/tokens' "uses" field: a blank
// value normalizes to 1 (no error); anything else must be a whole number in
// [fleetTokenUsesMin,fleetTokenUsesMax].
func parseFleetTokenUses(raw string) (uses int, normalized string, err error) {
	normalized = strings.TrimSpace(raw)
	if normalized == "" {
		return 1, "1", nil
	}
	n, convErr := strconv.Atoi(normalized)
	if convErr != nil || n < fleetTokenUsesMin || n > fleetTokenUsesMax {
		return 0, normalized, fmt.Errorf("uses must be a whole number from 1 to 100")
	}
	return n, normalized, nil
}

// parseFleetTags validates a comma-separated tags= field (shared by token
// creation and the per-node tags form): each non-empty, trimmed entry must
// match fleetTagRe, and at most fleetMaxTags may be given. A blank field
// (or one that's only commas/whitespace) is valid and yields no tags.
func parseFleetTags(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	tags := make([]string, 0, len(parts))
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t == "" {
			continue
		}
		if !fleetTagRe.MatchString(t) {
			return nil, fmt.Errorf("invalid tag %q (lowercase letters, digits, _ or -, max 32 characters)", t)
		}
		tags = append(tags, t)
	}
	if len(tags) > fleetMaxTags {
		return nil, fmt.Errorf("too many tags (max %d)", fleetMaxTags)
	}
	return tags, nil
}

// validateNodeName validates POST /fleet/nodes/{id}/rename's "name" field
// per the brief: 1..64 characters (runes, not bytes -- a multi-byte display
// name shouldn't be penalized for its UTF-8 encoding), no control
// characters.
func validateNodeName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	if utf8.RuneCountInString(name) > fleetNodeNameMax {
		return "", fmt.Errorf("name must be 64 characters or fewer")
	}
	for _, r := range name {
		// Cf covers bidi overrides (e.g. U+202E) that could make one node's
		// name render as another's in the admin table.
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return "", fmt.Errorf("name must not contain control or formatting characters")
		}
	}
	return name, nil
}

// fleetAPIErrStatus maps a core.FleetAPI error to the 4xx status the brief
// calls for ("FleetAPI errors ... render as a flash message ... with a 4xx
// status where it fits. Never return a 500."): core.ErrNoSuchNode,
// core.ErrNotMaster, and core.ErrNotFound (task C4 fix round 1 -- every
// incident/silence/maintenance-window/managed-fragment "no such X" lookup
// now wraps this) all mean "the thing this request named isn't there (any
// more)", so 404; anything else (a validation error the daemon itself
// rejected, e.g. registry.Update's "fleet: no node %s" for a stale id, or
// fleet.TokenStore's "no such token") is treated as a bad request, 400.
//
// This errors.Is check works identically whether Deps.Fleet() is backed
// in-process or by a control-socket control.Client: the latter's Client.call
// reconstructs each of these sentinels from the wire's plain-text error
// (internal/control/client.go's reconstructWireErr) precisely so this check
// keeps working across that hop too -- see that function's own doc.
func fleetAPIErrStatus(err error) int {
	if errors.Is(err, core.ErrNoSuchNode) || errors.Is(err, core.ErrNotMaster) || errors.Is(err, core.ErrNotFound) {
		return http.StatusNotFound
	}
	return http.StatusBadRequest
}

// TokenRow is one row of the join-tokens table: core.TokenView's fields
// flattened/formatted for templates/fleet_admin.html.
type TokenRow struct {
	ID       string
	UsesLeft int
	Expires  string
	Tags     string
	Creator  string
}

func newTokenRow(t core.TokenView) TokenRow {
	return TokenRow{
		ID:       t.ID,
		UsesLeft: t.Uses,
		// Expires routes through silenceTimeText (handlers_fleet_silences.go,
		// master-local zone with abbreviation) rather than its own RFC3339/
		// UTC convention -- round-2 review finding M2, folded into the same
		// cleanup as finding I1 so every absolute timestamp on the fleet
		// surface uses the one convention.
		Expires: silenceTimeText(t.Expires),
		Tags:    strings.Join(t.Tags, ","),
		Creator: t.Creator,
	}
}

// FleetAdminNodeRow is one row of both the node-management table and the
// link-health table on /fleet/admin -- both tables range over the SAME
// []FleetAdminNodeRow, since every field either comes straight from the
// roster (core.NodeSummary, embedded) or is a formatting/sticky-input
// helper at most one of the two tables needs.
type FleetAdminNodeRow struct {
	core.NodeSummary
	// RenameValue/TagsValue are the rename/tags forms' sticky input values:
	// the node's own current Name/Tags by default, overridden to the
	// rejected raw input (plus RenameErr/TagsErr) when THIS row is the one
	// a just-failed validation targeted -- see buildFleetAdminPageData.
	RenameValue string
	RenameErr   string
	TagsValue   string
	TagsErr     string
	// SkewText/SkewWarn/DropsWarn/OutboxText/OldestText mirror
	// FleetRow's identically-named fields (handlers_fleet.go): the CLI's
	// exact wording (fleetSkewText/fleetSkewWarnText/fleetDropsWarnText),
	// reused verbatim rather than reimplemented, per the brief's "reuse the
	// Task 5 helpers" ruling.
	SkewText   string
	SkewWarn   string
	DropsWarn  string
	OutboxText string
	OldestText string
	// CanRemove reports whether this node is eligible for POST
	// /fleet/nodes/{id}/remove: only a revoked or down node may be removed
	// (the brief's ruling) -- the template hides the remove control
	// entirely for anything else, and the handler re-checks this
	// server-side (never trusts the hidden-control-implies-safe assumption).
	CanRemove bool
}

func newFleetAdminNodeRow(n core.NodeSummary) FleetAdminNodeRow {
	outbox := ""
	if n.OutboxBytes > 0 {
		outbox = humanBytes(uint64(n.OutboxBytes))
	}
	oldest := ""
	if n.OutboxOldest > 0 {
		oldest = nodeAgoText(n.OutboxOldest)
	}
	return FleetAdminNodeRow{
		NodeSummary: n,
		RenameValue: n.Name,
		TagsValue:   strings.Join(n.Tags, ","),
		SkewText:    fleetSkewText(n),
		SkewWarn:    fleetSkewWarnText(n),
		DropsWarn:   fleetDropsWarnText(n),
		OutboxText:  outbox,
		OldestText:  oldest,
		CanRemove:   n.State == "down" || n.State == "revoked",
	}
}

// IssuedJoinToken is the freshly-minted join token's one-time render: the
// full `sudo trinetra fleet join <code>` command (task-7-brief.md's exact
// CLI shape, fleet_cmd.go's own fleetTokenCmd wording) plus the
// human-readable TTL/uses/tags it was minted with. Never persisted or
// logged anywhere beyond this one response -- see fleetTokenCreateHandler's
// SECURITY note.
type IssuedJoinToken struct {
	JoinCommand string
	TTL         string
	Uses        int
	Tags        string
}

// fleetAdminOptions is buildFleetAdminPageData's input: the page's
// transient, this-response-only state (a flash message, a freshly issued
// token, a form's sticky/invalid input) that a fresh GET /fleet/admin never
// carries (the zero value), but a POST mutation's re-rendered page/fragment
// does.
type fleetAdminOptions struct {
	Flash    string
	FlashErr bool
	Issued   *IssuedJoinToken

	TokenTTL  string
	TokenUses string
	TokenTags string
	TokenErr  string

	// NodeErrID names the ONE row (if any) a just-failed rename/tags
	// validation targeted; NodeRenameValue/NodeRenameErr and
	// NodeTagsValue/NodeTagsErr are that row's sticky input/message,
	// applied on top of newFleetAdminNodeRow's defaults in
	// buildFleetAdminPageData.
	NodeErrID       string
	NodeRenameValue string
	NodeRenameErr   string
	NodeTagsValue   string
	NodeTagsErr     string
}

// FleetAdminPageData is what templates/fleet_admin.html renders against.
type FleetAdminPageData struct {
	PageData
	Tokens []TokenRow
	Nodes  []FleetAdminNodeRow

	Flash    string
	FlashErr bool
	Issued   *IssuedJoinToken

	TokenTTL  string
	TokenUses string
	TokenTags string
	TokenErr  string
}

// fleetAdminNodesAndTokens reads the CURRENT roster and token list straight
// from d.Fleet() -- deliberately NOT through fleetMemoFrom(r)'s
// once-per-request cache (fleet_memo.go) that the rest of this package's
// fleet pages use. /fleet/admin is a one-off admin render, never the
// 5-second-polled hot path /fleet's memo exists to protect, and it MUST see
// its own just-applied mutation (a rename, a revoke, a new token) rather
// than whatever a validation lookup earlier in the SAME request already
// cached under the memo's sync.Once -- see e.g. fleetNodeRenameHandler,
// which looks up the pre-mutation node before calling RenameNode and then
// needs a POST-mutation roster to render.
func fleetAdminNodesAndTokens(d Deps) ([]core.NodeSummary, []core.TokenView) {
	fleet, err := fleetAPIFor(d)
	if err != nil {
		return nil, nil
	}
	nodes, _ := fleet.Nodes(core.NodeFilter{})
	tokens, _ := fleet.Tokens()
	return nodes, tokens
}

// buildFleetAdminPageData assembles FleetAdminPageData for GET /fleet/admin
// and every /fleet/tokens*|/fleet/nodes/* mutation's re-render: the current
// roster (excluding self -- there is nothing to manage about this host from
// its own admin page) and token list, plus opts' transient flash/sticky-
// form state layered on top.
func buildFleetAdminPageData(r *http.Request, d Deps, opts fleetAdminOptions) FleetAdminPageData {
	nodes, tokens := fleetAdminNodesAndTokens(d)

	rows := make([]FleetAdminNodeRow, 0, len(nodes))
	for _, n := range nodes {
		if n.Self {
			continue
		}
		row := newFleetAdminNodeRow(n)
		if opts.NodeErrID == n.ID {
			if opts.NodeRenameErr != "" {
				row.RenameValue = opts.NodeRenameValue
				row.RenameErr = opts.NodeRenameErr
			}
			if opts.NodeTagsErr != "" {
				row.TagsValue = opts.NodeTagsValue
				row.TagsErr = opts.NodeTagsErr
			}
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })

	tokRows := make([]TokenRow, 0, len(tokens))
	for _, t := range tokens {
		tokRows = append(tokRows, newTokenRow(t))
	}
	sort.Slice(tokRows, func(i, j int) bool { return tokRows[i].ID < tokRows[j].ID })

	tokenTTL := opts.TokenTTL
	if tokenTTL == "" {
		tokenTTL = fleetTokenTTLDefault
	}
	tokenUses := opts.TokenUses
	if tokenUses == "" {
		tokenUses = "1"
	}

	return FleetAdminPageData{
		PageData:  newPageData(r, d, "Fleet admin", "Nodes, join tokens, and link health"),
		Tokens:    tokRows,
		Nodes:     rows,
		Flash:     opts.Flash,
		FlashErr:  opts.FlashErr,
		Issued:    opts.Issued,
		TokenTTL:  tokenTTL,
		TokenUses: tokenUses,
		TokenTags: opts.TokenTags,
		TokenErr:  opts.TokenErr,
	}
}

// renderFleetAdminPage renders templates/fleet_admin.html's "content" block
// through the full app-shell layout (base.html), for GET /fleet/admin.
func renderFleetAdminPage(w http.ResponseWriter, data FleetAdminPageData, status int) error {
	tmpl, err := template.New("base.html").Funcs(funcMap).
		ParseFS(templatesFS, "templates/base.html", "templates/fleet_admin.html")
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	return tmpl.ExecuteTemplate(w, "base.html", data)
}

// renderFleetAdminFragment renders just fleet_admin.html's "content" block
// -- what every /fleet/tokens*|/fleet/nodes/* mutation responds with, so
// htmx (hx-target="#fleet-admin-page" hx-swap="outerHTML" on every form in
// the page, the same in-page pattern users.html uses) can swap the whole
// panel in place instead of a full page navigation. Also used to render an
// error response (a validation failure or a FleetAPI error) at whatever 4xx
// status the caller chooses -- never a redirect, per the brief.
func renderFleetAdminFragment(w http.ResponseWriter, data FleetAdminPageData, status int) error {
	tmpl, err := template.New("fleet_admin.html").Funcs(funcMap).
		ParseFS(templatesFS, "templates/fleet_admin.html")
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	return tmpl.ExecuteTemplate(w, "content", data)
}

// renderFleetAdminError re-renders the admin fragment with a flash message
// (FlashErr=true) at the given 4xx status -- the brief's "FleetAPI errors
// ... render as a flash message ... Never return a 500" ruling's shared
// implementation, used by every mutation handler's error path below.
func renderFleetAdminError(w http.ResponseWriter, r *http.Request, d Deps, msg string, status int) {
	data := buildFleetAdminPageData(r, d, fleetAdminOptions{Flash: msg, FlashErr: true})
	if err := renderFleetAdminFragment(w, data, status); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// fleetAdminPageHandler serves GET /fleet/admin: the join-token panel, node
// management table, and link-health table, admin+master gated
// (requireRole(RoleAdmin,...) at the route, fleetGateHTML here).
func fleetAdminPageHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fleetGateHTML(w, r, d) {
			return
		}
		data := buildFleetAdminPageData(r, d, fleetAdminOptions{})
		if err := renderFleetAdminPage(w, data, http.StatusOK); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// fleetTokenCreateHandler serves POST /fleet/tokens: validates ttl/uses/
// tags (parseFleetTokenTTL/Uses/parseFleetTags), mints a token via
// Fleet().CreateToken with Creator set to the acting admin's own user name,
// and re-renders the admin page directly (never a redirect) with the join
// command shown once.
//
// SECURITY: the join code (CreatedToken.JoinCode, prefixed "swj1_") is
// rendered into THIS ONE response and nowhere else -- logAudit's New field
// below carries only the ttl/uses/tags/creator summary, never JoinCode, and
// the response itself carries Cache-Control: no-store so neither a shared
// cache nor the browser's own back/forward cache retains a page holding it
// (task-7-brief.md: "the join code shows once, in the POST response").
func fleetTokenCreateHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fleetGateHTML(w, r, d) {
			return
		}
		if err := r.ParseForm(); err != nil {
			renderFleetAdminError(w, r, d, "invalid form", http.StatusBadRequest)
			return
		}

		ttl, ttlNorm, err := parseFleetTokenTTL(r.FormValue("ttl"))
		if err != nil {
			data := buildFleetAdminPageData(r, d, fleetAdminOptions{
				TokenTTL: ttlNorm, TokenUses: r.FormValue("uses"), TokenTags: r.FormValue("tags"),
				TokenErr: err.Error(),
			})
			if rerr := renderFleetAdminFragment(w, data, http.StatusBadRequest); rerr != nil {
				http.Error(w, rerr.Error(), http.StatusInternalServerError)
			}
			return
		}
		uses, usesNorm, err := parseFleetTokenUses(r.FormValue("uses"))
		if err != nil {
			data := buildFleetAdminPageData(r, d, fleetAdminOptions{
				TokenTTL: ttlNorm, TokenUses: usesNorm, TokenTags: r.FormValue("tags"),
				TokenErr: err.Error(),
			})
			if rerr := renderFleetAdminFragment(w, data, http.StatusBadRequest); rerr != nil {
				http.Error(w, rerr.Error(), http.StatusInternalServerError)
			}
			return
		}
		tags, err := parseFleetTags(r.FormValue("tags"))
		if err != nil {
			data := buildFleetAdminPageData(r, d, fleetAdminOptions{
				TokenTTL: ttlNorm, TokenUses: usesNorm, TokenTags: r.FormValue("tags"),
				TokenErr: err.Error(),
			})
			if rerr := renderFleetAdminFragment(w, data, http.StatusBadRequest); rerr != nil {
				http.Error(w, rerr.Error(), http.StatusInternalServerError)
			}
			return
		}

		fleet, err := fleetAPIFor(d)
		if err != nil {
			renderFleetAdminError(w, r, d, "fleet not available", http.StatusNotFound)
			return
		}
		creator := auditUser(r)
		created, err := fleet.CreateToken(core.TokenSpec{TTLSeconds: int64(ttl.Seconds()), Uses: uses, Tags: tags, Creator: creator})
		if err != nil {
			renderFleetAdminError(w, r, d, err.Error(), fleetAPIErrStatus(err))
			return
		}

		logAudit(d, r, "fleet.token.create", created.Token.ID, "",
			fmt.Sprintf("ttl=%s uses=%d tags=%s creator=%s", ttlNorm, uses, strings.Join(tags, ","), creator))

		issued := &IssuedJoinToken{
			JoinCommand: "sudo trinetra fleet join " + created.JoinCode,
			TTL:         ttlNorm,
			Uses:        uses,
			Tags:        strings.Join(tags, ","),
		}
		data := buildFleetAdminPageData(r, d, fleetAdminOptions{Issued: issued})
		w.Header().Set("Cache-Control", "no-store")
		if err := renderFleetAdminFragment(w, data, http.StatusOK); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// fleetTokenDeleteHandler serves POST /fleet/tokens/{id}/delete.
func fleetTokenDeleteHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fleetGateHTML(w, r, d) {
			return
		}
		id := r.PathValue("id")
		fleet, err := fleetAPIFor(d)
		if err != nil {
			renderFleetAdminError(w, r, d, "fleet not available", http.StatusNotFound)
			return
		}
		if err := fleet.DeleteToken(id, auditUser(r)); err != nil {
			renderFleetAdminError(w, r, d, err.Error(), fleetAPIErrStatus(err))
			return
		}
		logAudit(d, r, "fleet.token.delete", id, "", "")
		data := buildFleetAdminPageData(r, d, fleetAdminOptions{Flash: "token deleted"})
		if err := renderFleetAdminFragment(w, data, http.StatusOK); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// fleetNodeRenameHandler serves POST /fleet/nodes/{id}/rename. Rejects
// id=="self" with 400 (the brief: renaming this host happens through its
// own config, not the fleet admin page); a validation failure re-renders
// the fragment with the rejected value kept in the form.
func fleetNodeRenameHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fleetGateHTML(w, r, d) {
			return
		}
		id := r.PathValue("id")
		if id == core.SelfNodeID {
			renderFleetAdminError(w, r, d, "cannot manage this host from the fleet admin page", http.StatusBadRequest)
			return
		}
		if err := r.ParseForm(); err != nil {
			renderFleetAdminError(w, r, d, "invalid form", http.StatusBadRequest)
			return
		}
		raw := r.FormValue("name")
		name, err := validateNodeName(raw)
		if err != nil {
			data := buildFleetAdminPageData(r, d, fleetAdminOptions{
				NodeErrID: id, NodeRenameValue: raw, NodeRenameErr: err.Error(),
			})
			if rerr := renderFleetAdminFragment(w, data, http.StatusBadRequest); rerr != nil {
				http.Error(w, rerr.Error(), http.StatusInternalServerError)
			}
			return
		}

		fleet, err := fleetAPIFor(d)
		if err != nil {
			renderFleetAdminError(w, r, d, "fleet not available", http.StatusNotFound)
			return
		}
		oldName := ""
		if nodes, _ := fleet.Nodes(core.NodeFilter{}); nodes != nil {
			if n, ok := findNode(nodes, id); ok {
				oldName = n.Name
			}
		}
		if err := fleet.RenameNode(id, name, auditUser(r)); err != nil {
			renderFleetAdminError(w, r, d, err.Error(), fleetAPIErrStatus(err))
			return
		}
		logAudit(d, r, "fleet.node.rename", id, oldName, name)
		data := buildFleetAdminPageData(r, d, fleetAdminOptions{Flash: "renamed to " + name})
		if err := renderFleetAdminFragment(w, data, http.StatusOK); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// fleetNodeTagsHandler serves POST /fleet/nodes/{id}/tags.
func fleetNodeTagsHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fleetGateHTML(w, r, d) {
			return
		}
		id := r.PathValue("id")
		if id == core.SelfNodeID {
			renderFleetAdminError(w, r, d, "cannot manage this host from the fleet admin page", http.StatusBadRequest)
			return
		}
		if err := r.ParseForm(); err != nil {
			renderFleetAdminError(w, r, d, "invalid form", http.StatusBadRequest)
			return
		}
		raw := r.FormValue("tags")
		tags, err := parseFleetTags(raw)
		if err != nil {
			data := buildFleetAdminPageData(r, d, fleetAdminOptions{
				NodeErrID: id, NodeTagsValue: raw, NodeTagsErr: err.Error(),
			})
			if rerr := renderFleetAdminFragment(w, data, http.StatusBadRequest); rerr != nil {
				http.Error(w, rerr.Error(), http.StatusInternalServerError)
			}
			return
		}

		fleet, err := fleetAPIFor(d)
		if err != nil {
			renderFleetAdminError(w, r, d, "fleet not available", http.StatusNotFound)
			return
		}
		oldTags := ""
		if nodes, _ := fleet.Nodes(core.NodeFilter{}); nodes != nil {
			if n, ok := findNode(nodes, id); ok {
				oldTags = strings.Join(n.Tags, ",")
			}
		}
		if err := fleet.SetNodeTags(id, tags, auditUser(r)); err != nil {
			renderFleetAdminError(w, r, d, err.Error(), fleetAPIErrStatus(err))
			return
		}
		logAudit(d, r, "fleet.node.tags", id, oldTags, strings.Join(tags, ","))
		data := buildFleetAdminPageData(r, d, fleetAdminOptions{Flash: "tags updated"})
		if err := renderFleetAdminFragment(w, data, http.StatusOK); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// fleetNodeRevokeHandler serves POST /fleet/nodes/{id}/revoke. The UI's
// two-step in-page confirm (fleet_admin.html's checkbox-toggle pattern) is
// purely client-side; this handler itself has no notion of "confirmed" --
// every POST that reaches it (past requireCSRF) revokes.
func fleetNodeRevokeHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fleetGateHTML(w, r, d) {
			return
		}
		id := r.PathValue("id")
		if id == core.SelfNodeID {
			renderFleetAdminError(w, r, d, "cannot revoke this host", http.StatusBadRequest)
			return
		}
		fleet, err := fleetAPIFor(d)
		if err != nil {
			renderFleetAdminError(w, r, d, "fleet not available", http.StatusNotFound)
			return
		}
		if err := fleet.RevokeNode(id, auditUser(r)); err != nil {
			renderFleetAdminError(w, r, d, err.Error(), fleetAPIErrStatus(err))
			return
		}
		logAudit(d, r, "fleet.node.revoke", id, "", "revoked")
		data := buildFleetAdminPageData(r, d, fleetAdminOptions{Flash: "node revoked"})
		if err := renderFleetAdminFragment(w, data, http.StatusOK); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// fleetNodeRemoveHandler serves POST /fleet/nodes/{id}/remove: only a
// revoked or down node may be removed (the brief's ruling; the template
// hides the control otherwise, but this is the actual enforcement). A node
// this daemon's roster no longer recognizes falls through to
// Fleet().RemoveNode itself, whose own error (core.ErrNoSuchNode or
// equivalent) is what renders the 404 flash -- the state check only
// applies when the id IS still a known node.
func fleetNodeRemoveHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fleetGateHTML(w, r, d) {
			return
		}
		id := r.PathValue("id")
		if id == core.SelfNodeID {
			renderFleetAdminError(w, r, d, "cannot remove this host", http.StatusBadRequest)
			return
		}
		fleet, err := fleetAPIFor(d)
		if err != nil {
			renderFleetAdminError(w, r, d, "fleet not available", http.StatusNotFound)
			return
		}
		var oldSummary string
		if nodes, _ := fleet.Nodes(core.NodeFilter{}); nodes != nil {
			if n, ok := findNode(nodes, id); ok {
				if n.State != "down" && n.State != "revoked" {
					renderFleetAdminError(w, r, d, "remove is only available for revoked or down nodes", http.StatusBadRequest)
					return
				}
				oldSummary = fmt.Sprintf("name=%s state=%s", n.Name, n.State)
			}
		}
		if err := fleet.RemoveNode(id, auditUser(r)); err != nil {
			renderFleetAdminError(w, r, d, err.Error(), fleetAPIErrStatus(err))
			return
		}
		logAudit(d, r, "fleet.node.remove", id, oldSummary, "")
		data := buildFleetAdminPageData(r, d, fleetAdminOptions{Flash: "node removed; its history stays on disk"})
		if err := renderFleetAdminFragment(w, data, http.StatusOK); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}
