package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Source interface {
	ReleaseAsset(ctx context.Context, version, name string) (io.ReadCloser, error)
	ChannelAsset(ctx context.Context, name string) (io.ReadCloser, error)
}

var ErrNoChannel = errors.New("update: source has no channel pointers")

// ErrNotFound marks a source answer that the asset (or its release) does not
// exist, as opposed to a transport failure: FetchLatest turns it into
// ErrNoPointer, which the freeze alert treats as a withheld pointer.
var ErrNotFound = errors.New("update: not found")

// GitHubSource reads release assets through the GitHub REST API, so it works
// for a private repo with a read-only token and for a public one without.
// Authenticity never comes from here: every byte is verified by the caller.
type GitHubSource struct {
	Repo    string // "InfoDiveLabs/trinetra"
	Token   string
	BaseURL string // default https://api.github.com
	Client  *http.Client
}

func (g GitHubSource) client() *http.Client {
	if g.Client != nil {
		return g.Client
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

func (g GitHubSource) base() string {
	if g.BaseURL != "" {
		return strings.TrimRight(g.BaseURL, "/")
	}
	return "https://api.github.com"
}

func (g GitHubSource) get(ctx context.Context, url, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	resp, err := g.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("update: GET %s: %w", url, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, fmt.Errorf("update: GET %s: %s: %w", url, resp.Status, ErrNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("update: GET %s: %s", url, resp.Status)
	}
	return resp, nil
}

func (g GitHubSource) asset(ctx context.Context, tag, name string) (io.ReadCloser, error) {
	resp, err := g.get(ctx, g.base()+"/repos/"+g.Repo+"/releases/tags/"+tag, "application/vnd.github+json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var rel struct {
		Assets []struct{ Name, URL string } `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("update: release %s: %w", tag, err)
	}
	for _, a := range rel.Assets {
		if a.Name == name {
			// Go's client drops Authorization on the cross-host redirect to
			// GitHub's storage host, which is what we want.
			r, err := g.get(ctx, a.URL, "application/octet-stream")
			if err != nil {
				return nil, err
			}
			return r.Body, nil
		}
	}
	return nil, fmt.Errorf("update: release %s has no asset %q: %w", tag, name, ErrNotFound)
}

func (g GitHubSource) ReleaseAsset(ctx context.Context, version, name string) (io.ReadCloser, error) {
	return g.asset(ctx, "v"+strings.TrimPrefix(version, "v"), name)
}

func (g GitHubSource) ChannelAsset(ctx context.Context, name string) (io.ReadCloser, error) {
	return g.asset(ctx, "channels", name)
}

// DirSource reads a local release bundle directory (manifest, both
// signatures and binaries side by side). The version argument is ignored:
// a bundle holds exactly one release, and its manifest says which.
type DirSource struct{ Dir string }

func (d DirSource) ReleaseAsset(_ context.Context, _ string, name string) (io.ReadCloser, error) {
	if !validAssetName(name) {
		return nil, fmt.Errorf("update: bad asset name %q", name)
	}
	return os.Open(filepath.Join(d.Dir, name))
}

func (d DirSource) ChannelAsset(context.Context, string) (io.ReadCloser, error) {
	return nil, ErrNoChannel
}
