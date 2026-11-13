package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxSigBytes = 4 << 10

func (r *Registry) httpClient() *http.Client {
	c := http.Client{Timeout: 30 * time.Second}
	if r.o.HTTP != nil {
		c = *r.o.HTTP
	}
	if c.CheckRedirect == nil {
		c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" {
				return errors.New("registry: redirect to a non-https URL refused")
			}
			if len(via) >= 5 {
				return errors.New("registry: too many redirects")
			}
			return nil
		}
	}
	return &c
}

func (r *Registry) get(ctx context.Context, c *http.Client, u string, limit int64) ([]byte, error) {
	pu, err := url.Parse(u)
	if err != nil || pu.Scheme != "https" || pu.Host == "" {
		return nil, fmt.Errorf("registry: %q is not an https URL", u)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registry: GET %s: %s", u, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("registry: GET %s: body larger than %d bytes", u, limit)
	}
	return b, nil
}

func (r *Registry) fetchIndex(ctx context.Context) (index, sig []byte, err error) {
	base := ""
	if r.o.BaseURL != nil {
		base = r.o.BaseURL()
	}
	if base == "" {
		return nil, nil, errors.New("registry: no registry URL configured")
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	c := r.httpClient()
	if index, err = r.get(ctx, c, base+"index.json", MaxIndexBytes); err != nil {
		return nil, nil, err
	}
	if sig, err = r.get(ctx, c, base+"index.json.sig", maxSigBytes); err != nil {
		return nil, nil, err
	}
	return index, sig, nil
}
