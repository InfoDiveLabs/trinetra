// internal/update/source_test.go
package update

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitHubSourceUsesTokenAndOctetStream(t *testing.T) {
	var sawAuth, sawAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/InfoDiveLabs/trinetra/releases/tags/v0.5.0":
			io.WriteString(w, `{"assets":[{"name":"manifest.json","url":"`+"http://"+r.Host+`/assets/7"}]}`)
		case "/assets/7":
			sawAuth, sawAccept = r.Header.Get("Authorization"), r.Header.Get("Accept")
			io.WriteString(w, "MANIFEST")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	src := GitHubSource{Repo: "InfoDiveLabs/trinetra", Token: "tok123", BaseURL: srv.URL, Client: srv.Client()}
	rc, err := src.ReleaseAsset(context.Background(), "0.5.0", "manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "MANIFEST" || sawAuth != "Bearer tok123" || sawAccept != "application/octet-stream" {
		t.Fatalf("body=%q auth=%q accept=%q", b, sawAuth, sawAccept)
	}
	if _, err := src.ReleaseAsset(context.Background(), "0.5.0", "missing"); err == nil || strings.Contains(err.Error(), "tok123") {
		t.Fatalf("missing asset err = %v (must fail and never echo the token)", err)
	}
}

func TestDirSourceRejectsUnsafeNames(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}

	src := DirSource{Dir: dir}
	rc, err := src.ReleaseAsset(context.Background(), "", "manifest.json")
	if err != nil {
		t.Fatalf("valid name refused: %v", err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "ok" {
		t.Fatalf("valid name body = %q", b)
	}

	for _, name := range []string{"", ".", "..", "../x", "a/b", `a\b`} {
		if rc, err := src.ReleaseAsset(context.Background(), "", name); err == nil {
			rc.Close()
			t.Errorf("%q: accepted (must refuse and never open anything outside Dir)", name)
		}
	}
}
