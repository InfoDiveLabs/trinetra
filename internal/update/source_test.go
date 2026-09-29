// internal/update/source_test.go
package update

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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
