package web

import (
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"time"
)

// usersMutation composes requireRole(RoleAdmin, ...) with requireCSRF: every
// /users/* mutation (invite issue, role change, remove, credential revoke)
// needs BOTH gates -- only an admin session may reach it (requireRole), and
// only with a valid CSRF token (requireCSRF) -- unlike POST /logout
// (routes.go), which only needs the latter, since these mutate someone
// ELSE's account rather than the caller's own session.
func usersMutation(d Deps, next http.HandlerFunc) http.HandlerFunc {
	return requireRole(RoleAdmin, d, func(w http.ResponseWriter, r *http.Request) {
		requireCSRF(next).ServeHTTP(w, r)
	})
}

// CredentialRow is one passkey in a UserRow's Passkeys column. Param is the
// base64url encoding of the raw Credential.ID (see credentialParam) used in
// the revoke form's URL -- a raw credential ID is arbitrary bytes, not safe
// to drop straight into a URL path segment. Label is a short, non-secret
// display hint derived from the same value (there's no per-device nickname
// in the design doc's users.json shape to show instead).
type CredentialRow struct {
	Param string
	Label string
}

// credentialParam/credentialFromParam convert a Credential.ID to/from the
// URL-safe form the revoke route's {credParam} path segment carries.
// RawURLEncoding is used (no padding, '-'/'_' alphabet) so the result is
// always a single clean path segment.
func credentialParam(id []byte) string {
	return base64.RawURLEncoding.EncodeToString(id)
}

func credentialFromParam(param string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(param)
}

// UserRow is one row of the /users table: a *User's fields (plus its
// Credentials) flattened/formatted for templates/users.html.
type UserRow struct {
	ID          string
	Name        string
	Role        Role
	Created     string
	IsSelf      bool
	Credentials []CredentialRow
}

// IssuedInvite is the freshly-minted enrollment link usersInviteHandler
// renders after Issue succeeds, so the admin can copy/share it before it's
// redeemed. Never persisted -- it only exists for the lifetime of the one
// HTTP response that just issued it; a page reload (plain GET /users) shows
// no such panel, which is why usersPageHandler always passes nil for this.
type IssuedInvite struct {
	Link string
	Role Role
	// TTL is the raw <select> value (e.g. "1h"), carried into the "Re-issue"
	// button's hidden field so re-issuing repeats the same role/ttl choice.
	TTL string
	// TTLLabel is TTL's human-readable form (e.g. "1 hour").
	TTLLabel string
}

// UsersPageData is what templates/users.html renders against: the shared
// PageData (nav/topbar/CSRF) embedded, plus this page's own state -- the
// current roster and, if an admin just (re-)issued one, the freshly-minted
// enrollment link.
type UsersPageData struct {
	PageData
	Users  []UserRow
	Issued *IssuedInvite
}

// inviteTTLs is the invite form's fixed set of expiries, mirroring the
// mockup's "Link expires" <select> (ui-mockup/users.html).
var inviteTTLs = []struct{ Value, Label string }{
	{"1h", "1 hour"},
	{"24h", "24 hours"},
	{"168h", "7 days"},
}

// ttlLabel maps an invite TTL <select> value to its human label, falling
// back to the raw value itself for anything not in inviteTTLs (defensive;
// the <select> only ever offers these three, but a hand-crafted POST could
// send something else -- see usersInviteHandler's parsing).
func ttlLabel(value string) string {
	for _, t := range inviteTTLs {
		if t.Value == value {
			return t.Label
		}
	}
	return value
}

// buildUsersPageData assembles UsersPageData from the current store state
// (and, if issued is non-nil, the just-minted invite to render alongside
// it), tagging the requesting session's own account with IsSelf so the
// template can show "(you)" the way the mockup does.
func buildUsersPageData(r *http.Request, d Deps, store UserStore, issued *IssuedInvite) UsersPageData {
	self, _ := userFromContext(r)
	all := store.List()
	rows := make([]UserRow, 0, len(all))
	for _, u := range all {
		creds := make([]CredentialRow, 0, len(u.Credentials))
		for _, c := range u.Credentials {
			param := credentialParam(c.ID)
			label := param
			if len(label) > 10 {
				label = label[:10]
			}
			creds = append(creds, CredentialRow{Param: param, Label: label})
		}
		rows = append(rows, UserRow{
			ID:          u.ID,
			Name:        u.Name,
			Role:        u.Role,
			Created:     time.Unix(u.Created, 0).UTC().Format("2006-01-02"),
			IsSelf:      self != nil && self.ID == u.ID,
			Credentials: creds,
		})
	}
	return UsersPageData{
		PageData: newPageData(r, d, "Users", "Accounts, roles, and enrollment tokens"),
		Users:    rows,
		Issued:   issued,
	}
}

// renderUsersPage renders templates/users.html through the full app-shell
// layout (base.html) -- the same parse/execute shape as renderPage
// (templates.go), but for UsersPageData rather than the plain PageData every
// other page uses today, since this is the first admin page needing extra
// fields (Users, Issued) alongside the shared nav/topbar ones.
func renderUsersPage(w http.ResponseWriter, data UsersPageData) error {
	tmpl, err := template.New("base.html").Funcs(funcMap).
		ParseFS(templatesFS, "templates/base.html", "templates/users.html")
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	return tmpl.ExecuteTemplate(w, "base.html", data)
}

// renderUsersFragment renders just users.html's "content" block, without the
// base.html shell around it -- what every /users/* mutation responds with, so
// htmx (hx-target="#users-page" hx-swap="outerHTML" on each row/invite form)
// can swap the roster/invite panel in place instead of a full page
// navigation.
func renderUsersFragment(w http.ResponseWriter, data UsersPageData) error {
	tmpl, err := template.New("users.html").Funcs(funcMap).
		ParseFS(templatesFS, "templates/users.html")
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	return tmpl.ExecuteTemplate(w, "content", data)
}

// usersPageHandler renders GET /users: the full roster plus the invite/roles
// panels, through the app shell. requireRole(RoleAdmin, ...) (routes.go's
// wiring) has already gated this by the time it runs.
func usersPageHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		store := newUserStore(d.StateDir)
		data := buildUsersPageData(r, d, store, nil)
		if err := renderUsersPage(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// defaultInviteTTL is used whenever POST /users/invite's "ttl" field is
// missing or fails to parse as a duration.
const defaultInviteTTL = time.Hour

// usersInviteHandler issues (or re-issues) a single-use enrollment token for
// the posted role/ttl (tokenStore.Issue, enroll_tokens.go) and renders the
// users fragment with the resulting /enroll?token=... link so the admin can
// copy/share it. Re-issuing is just calling this again -- a single-use token
// left outstanding after an abandoned ceremony (see enroll_tokens.go's
// Redeem doc) is never revoked, only ever left to expire or be redeemed;
// this endpoint has no notion of "the previous token", it only ever mints a
// fresh one.
func usersInviteHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		role := Role(r.FormValue("role"))
		if role != RoleAdmin && role != RoleViewer {
			http.Error(w, "role must be admin or viewer", http.StatusBadRequest)
			return
		}
		ttlValue := r.FormValue("ttl")
		ttl, err := time.ParseDuration(ttlValue)
		if err != nil || ttl <= 0 {
			ttlValue = "1h"
			ttl = defaultInviteTTL
		}

		tok := newTokenStore(d.StateDir).Issue(role, ttl)
		if tok == "" {
			http.Error(w, "could not issue enrollment token", http.StatusInternalServerError)
			return
		}

		issued := &IssuedInvite{
			Link:     "/enroll?token=" + tok,
			Role:     role,
			TTL:      ttlValue,
			TTLLabel: ttlLabel(ttlValue),
		}
		logAudit(d, r, "user.invite", string(role), "", "ttl="+ttlValue)
		store := newUserStore(d.StateDir)
		data := buildUsersPageData(r, d, store, issued)
		if err := renderUsersFragment(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// usersRoleHandler changes {id}'s role to the posted "role" value
// (admin|viewer). Refuses (409) to demote the sole remaining admin to
// viewer -- the same lockout usersRemoveHandler's guard closes for removal:
// with zero admins left, /users (and every other admin route) becomes
// permanently unreachable, since a fresh first-run bootstrap admin only
// happens when the user store is fully EMPTY (jsonUserStore.
// CreateFirstAdmin), not merely admin-less.
func usersRoleHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		newRole := Role(r.FormValue("role"))
		if newRole != RoleAdmin && newRole != RoleViewer {
			http.Error(w, "role must be admin or viewer", http.StatusBadRequest)
			return
		}

		store := newUserStore(d.StateDir)
		oldRole := ""
		if u, ok := store.Get(id); ok {
			oldRole = string(u.Role)
		}
		switch err := store.SetRoleUnlessLastAdmin(id, newRole); {
		case errors.Is(err, errUserNotFound):
			http.Error(w, "user not found", http.StatusNotFound)
			return
		case errors.Is(err, errLastAdmin):
			http.Error(w, "refusing to demote the last remaining admin", http.StatusConflict)
			return
		case err != nil:
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		logAudit(d, r, "user.role", id, oldRole, string(newRole))

		data := buildUsersPageData(r, d, store, nil)
		if err := renderUsersFragment(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// usersRemoveHandler deletes {id}, refusing (409) to remove the sole
// remaining admin -- see usersRoleHandler's doc for why that lockout matters
// (this is the guard the task brief specifically calls out).
func usersRemoveHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		store := newUserStore(d.StateDir)
		oldSummary := ""
		if u, ok := store.Get(id); ok {
			oldSummary = fmt.Sprintf("name=%s role=%s", u.Name, u.Role)
		}
		switch err := store.RemoveUnlessLastAdmin(id); {
		case errors.Is(err, errUserNotFound):
			http.Error(w, "user not found", http.StatusNotFound)
			return
		case errors.Is(err, errLastAdmin):
			http.Error(w, "refusing to remove the last remaining admin", http.StatusConflict)
			return
		case err != nil:
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		logAudit(d, r, "user.remove", id, oldSummary, "")

		data := buildUsersPageData(r, d, store, nil)
		if err := renderUsersFragment(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// usersRevokeCredentialHandler removes exactly one credential ({credParam},
// see credentialFromParam) from {id}'s Credentials, leaving every other
// credential -- this user's or anyone else's -- untouched. Refuses (409) to
// revoke the sole remaining admin's last credential -- see
// UserStore.RevokeCredentialUnlessLastAdmin's doc for why: it's the third
// zero-admin lockout vector, alongside usersRoleHandler's demote guard and
// usersRemoveHandler's remove guard, and the atomic store method (rather
// than this handler's old Get-then-Put) is also what closes the
// read-modify-write race against a concurrent finishLogin.
func usersRevokeCredentialHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		want, err := credentialFromParam(r.PathValue("credParam"))
		if err != nil {
			http.Error(w, "invalid credential", http.StatusBadRequest)
			return
		}
		revoked := credentialParam(want)

		store := newUserStore(d.StateDir)
		switch err := store.RevokeCredentialUnlessLastAdmin(id, string(want)); {
		case errors.Is(err, errUserNotFound):
			http.Error(w, "user not found", http.StatusNotFound)
			return
		case errors.Is(err, errCredentialNotFound):
			http.Error(w, "credential not found", http.StatusNotFound)
			return
		case errors.Is(err, errLastAdminCredential):
			http.Error(w, "refusing to revoke the last remaining admin's last credential", http.StatusConflict)
			return
		case err != nil:
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		logAudit(d, r, "user.credential.revoke", id, revoked, "")

		data := buildUsersPageData(r, d, store, nil)
		if err := renderUsersFragment(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}
