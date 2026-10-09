// Package web: handlers_updates.go is the admin-only Updates page: GET /updates shows this
// host's self-update posture.
package web

import (
	"html/template"
	"net/http"
	"net/url"
	"strings"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// updatesMutation composes requireRole(RoleAdmin, ...) with requireCSRF, mirroring
// configMutation (handlers_config.go): only an admin session may POST /updates/*.
func updatesMutation(d Deps, next http.HandlerFunc) http.HandlerFunc {
	return requireRole(RoleAdmin, d, func(w http.ResponseWriter, r *http.Request) {
		requireCSRF(next).ServeHTTP(w, r)
	})
}

// UpdatesPageData is what templates/updates.html's "content" block renders against.
type UpdatesPageData struct {
	PageData

	Status core.UpdateStatusView

	Flash    string
	FlashErr bool
}

// updatesPageOptions lets a POST handler that re-renders the page in place.
type updatesPageOptions struct {
	Flash    string
	FlashErr bool
}

// resolveUpdatesFlash maps ?flash= to display text: a FIXED set of codes only (this file's
// own top doc) -- every value below is a literal the handler chose.
func resolveUpdatesFlash(r *http.Request) (text string, isErr bool) {
	switch r.URL.Query().Get("flash") {
	case "update-checked":
		return "Checked for updates.", false
	case "update-started":
		return "Update started; the health guard will confirm it shortly.", false
	case "rollback-started":
		return "Rollback started; the health guard will confirm it shortly.", false
	case "update-error":
		return "The update action failed. See the daemon log for details.", true
	case "update-busy":
		return "Another update is already in progress on this host. Wait for it to be confirmed or rolled back, then try again.", true
	}
	return "", false
}

// buildUpdatesPageData assembles UpdatesPageData: the live status.
func buildUpdatesPageData(r *http.Request, d Deps, opts updatesPageOptions) UpdatesPageData {
	var status core.UpdateStatusView
	statusFailed := false
	if d.API != nil {
		var err error
		status, err = d.API.UpdateStatus()
		if err != nil {
			statusFailed = true
		}
	} else {
		statusFailed = true
	}

	flash, flashErr := opts.Flash, opts.FlashErr
	if flash == "" {
		flash, flashErr = resolveUpdatesFlash(r)
	}
	if flash == "" && statusFailed {
		flash, flashErr = "The update action failed. See the daemon log for details.", true
	}

	return UpdatesPageData{
		PageData: newPageData(r, d, "Updates", "Signed release channel, status, and manual update actions"),
		Status:   status,
		Flash:    flash,
		FlashErr: flashErr,
	}
}

// renderUpdatesPage renders templates/updates.html through the full
// app-shell layout, mirroring renderConfigPage/renderSilencesPage.
func renderUpdatesPage(w http.ResponseWriter, data UpdatesPageData) error {
	tmpl, err := template.New("base.html").Funcs(funcMap).
		ParseFS(templatesFS, "templates/base.html", "templates/updates.html")
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	return tmpl.ExecuteTemplate(w, "base.html", data)
}

// updateErrorFlash picks the fixed flash code for a failed apply/rollback: "update-busy"
// when the host refused because another update holds its lock or is pending.
func updateErrorFlash(err error) string {
	if strings.Contains(err.Error(), "already in progress") {
		return "update-busy"
	}
	return "update-error"
}

// redirectToUpdates redirects to GET /updates with a fixed ?flash= code (or
// none at all, when flashCode is ""), mirroring redirectToSilences.
func redirectToUpdates(w http.ResponseWriter, r *http.Request, flashCode string) {
	target := "/updates"
	if flashCode != "" {
		v := url.Values{"flash": {flashCode}}
		target += "?" + v.Encode()
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// updatesPageHandler renders GET /updates. requireRole(RoleAdmin, ...)
// (routes.go's wiring) has already gated this by the time it runs.
func updatesPageHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := buildUpdatesPageData(r, d, updatesPageOptions{})
		if err := renderUpdatesPage(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// isUpToDateCheckErr reports whether err is UpdateCheck's ordinary "the channel's release
// is already installed" outcome.
func isUpToDateCheckErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "already installed")
}

// updatesCheckHandler handles POST /updates/check: fetch/verify the channel's latest
// release and record the outcome (core.API.UpdateCheck).
func updatesCheckHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.API == nil {
			redirectToUpdates(w, r, "update-error")
			return
		}
		if _, err := d.API.UpdateCheck(r.Context()); err != nil && !isUpToDateCheckErr(err) {
			redirectToUpdates(w, r, "update-error")
			return
		}
		redirectToUpdates(w, r, "update-checked")
	}
}

// updatesApplyHandler handles POST /updates/apply: install the posted version.
func updatesApplyHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			redirectToUpdates(w, r, "update-error")
			return
		}
		version := r.FormValue("version")
		if d.API == nil {
			redirectToUpdates(w, r, "update-error")
			return
		}
		if err := d.API.UpdateApply(r.Context(), version); err != nil {
			redirectToUpdates(w, r, updateErrorFlash(err))
			return
		}
		logAudit(d, r, "update.apply", "version", "", version)
		redirectToUpdates(w, r, "update-started")
	}
}

// updatesRollbackHandler handles POST /updates/rollback: restore the previous build and
// start its health guard (core.API.UpdateRollback).
func updatesRollbackHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.API == nil {
			redirectToUpdates(w, r, "update-error")
			return
		}
		if err := d.API.UpdateRollback(); err != nil {
			redirectToUpdates(w, r, updateErrorFlash(err))
			return
		}
		logAudit(d, r, "update.rollback", "version", "", "")
		redirectToUpdates(w, r, "rollback-started")
	}
}
