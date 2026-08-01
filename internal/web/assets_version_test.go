package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAssetURLIsVersioned confirms the template "asset" helper appends the
// content-hash ?v= query used for cache-busting.
func TestAssetURLIsVersioned(t *testing.T) {
	if assetVersion == "" {
		t.Fatal("assetVersion is empty -- hash over embedded assets failed")
	}
	got := assetURL("/assets/app.js")
	if !strings.HasPrefix(got, "/assets/app.js?v=") {
		t.Fatalf("assetURL = %q, want /assets/app.js?v=<hash>", got)
	}
}

// TestRenderedPageReferencesVersionedAssets confirms the emitted HTML uses the
// versioned asset URLs (so a stale CDN copy of the bare URL is never fetched).
func TestRenderedPageReferencesVersionedAssets(t *testing.T) {
	h := newHandler(enrollTestDeps(t))
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "/assets/app.js?v="+assetVersion) {
		t.Errorf("login page does not reference versioned app.js (v=%s)\n%s", assetVersion, body)
	}
}

// TestAssetsServedImmutable confirms asset responses carry a long-lived
// immutable Cache-Control (safe because URLs are content-hashed).
func TestAssetsServedImmutable(t *testing.T) {
	h := newHandler(enrollTestDeps(t))
	req := httptest.NewRequest(http.MethodGet, "/assets/app.js", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /assets/app.js = %d, want 200", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q, want immutable", cc)
	}
}
