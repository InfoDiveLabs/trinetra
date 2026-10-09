// Command relsrv is a stdlib-only stand-in for the parts of the GitHub REST API
// internal/update.GitHubSource talks to, used by the update-e2e docker harness.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

type asset struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// server bundles the fixture directory, the base URL this process is reachable at (so asset
// URLs it hands out resolve back to itself).
type server struct {
	dir     string
	baseURL string
	token   string
}

func (s *server) authorized(r *http.Request) bool {
	want := "Bearer " + s.token
	return r.Header.Get("Authorization") == want
}

// releaseTags handles GET /repos/InfoDiveLabs/trinetra/releases/tags/{tag}: lists every
// regular file directly under RELEASES_DIR/<tag>/ as a release asset.
func (s *server) releaseTags(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	tag := r.PathValue("tag")
	dir := filepath.Join(s.dir, tag)
	entries, err := os.ReadDir(dir)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var assets []asset
	for _, e := range entries {
		if e.Type().IsRegular() {
			assets = append(assets, asset{
				Name: e.Name(),
				URL:  s.baseURL + "/assets/" + tag + "/" + e.Name(),
			})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"assets": assets})
}

// assetFile handles GET /assets/{tag}/{name}: the raw bytes of RELEASES_DIR/<tag>/<name>.
// filepath.Join cleans ".." segments.
func (s *server) assetFile(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	tag := r.PathValue("tag")
	name := r.PathValue("name")
	dir := filepath.Join(s.dir, tag)
	path := filepath.Join(dir, name)
	if !strings.HasPrefix(path, dir+string(filepath.Separator)) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, path)
}

func newMux(s *server) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/InfoDiveLabs/trinetra/releases/tags/{tag}", s.releaseTags)
	mux.HandleFunc("GET /assets/{tag}/{name}", s.assetFile)
	return mux
}

func main() {
	dir := os.Getenv("RELEASES_DIR")
	if dir == "" {
		dir = "/releases"
	}
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	base := os.Getenv("BASE_URL")
	if base == "" {
		base = "http://relsrv:8080"
	}
	token := os.Getenv("TOKEN")
	if token == "" {
		token = "e2etoken"
	}
	s := &server{dir: dir, baseURL: base, token: token}
	log.Fatal(http.ListenAndServe(addr, newMux(s)))
}
