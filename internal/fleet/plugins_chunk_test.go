package fleet

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"testing"
)

type nopCloseReadSeeker struct{ *bytes.Reader }

func (nopCloseReadSeeker) Close() error { return nil }

type fakePkgSource struct {
	node string
	sha  string
	data []byte
}

func (s *fakePkgSource) Open(nodeID, sha string) (io.ReadSeekCloser, int64, error) {
	if nodeID != s.node || sha != s.sha {
		return nil, 0, errors.New("no offer")
	}
	return nopCloseReadSeeker{bytes.NewReader(s.data)}, int64(len(s.data)), nil
}

func chunkGet(t *testing.T, c *http.Client, base, sha string, off int64) (*http.Response, []byte) {
	t.Helper()
	resp, err := c.Get(base + PathPluginPackage + sha + "?offset=" + strconv.FormatInt(off, 10))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func TestPluginChunkRequiresOffer(t *testing.T) {
	src := &fakePkgSource{node: "someone-else", sha: "abc", data: []byte("data")}
	f := newMasterFixture(t, func(c *MasterConfig) { c.PluginPackages = src })
	_, c := f.join(t, nil)
	if resp, _ := chunkGet(t, c, f.srv.URL, "abc", 0); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want 404 for a node without an offer", resp.StatusCode)
	}
	f2 := newMasterFixture(t)
	_, c2 := f2.join(t, nil)
	if resp, _ := chunkGet(t, c2, f2.srv.URL, "abc", 0); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want 404 with no source configured", resp.StatusCode)
	}
}

func TestPluginChunkServesRangesAndEOF(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789"), PluginChunkSize/10+1000)
	data = data[:PluginChunkSize+5000]
	src := &fakePkgSource{sha: "abc", data: data}
	f := newMasterFixture(t, func(c *MasterConfig) { c.PluginPackages = src })
	id, c := f.join(t, nil)
	src.node = id

	resp, b := chunkGet(t, c, f.srv.URL, "abc", 0)
	if resp.StatusCode != 200 || len(b) != PluginChunkSize || !bytes.Equal(b, data[:PluginChunkSize]) {
		t.Fatalf("first chunk: status %d len %d", resp.StatusCode, len(b))
	}
	if resp.Header.Get(HeaderPluginSize) != strconv.Itoa(len(data)) {
		t.Fatalf("size header = %q", resp.Header.Get(HeaderPluginSize))
	}
	resp, b = chunkGet(t, c, f.srv.URL, "abc", PluginChunkSize)
	if resp.StatusCode != 200 || !bytes.Equal(b, data[PluginChunkSize:]) {
		t.Fatalf("tail chunk: status %d len %d", resp.StatusCode, len(b))
	}
	resp, b = chunkGet(t, c, f.srv.URL, "abc", int64(len(data)))
	if resp.StatusCode != 200 || len(b) != 0 {
		t.Fatalf("eof: status %d len %d", resp.StatusCode, len(b))
	}
	if resp, _ = chunkGet(t, c, f.srv.URL, "abc", int64(len(data))+1); resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("past eof: status %d, want 416", resp.StatusCode)
	}
	if resp, _ = chunkGet(t, c, f.srv.URL, "abc", -1); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("negative offset: status %d, want 400", resp.StatusCode)
	}
}

func TestPluginChunkRequiresClientCert(t *testing.T) {
	f := newMasterFixture(t, func(c *MasterConfig) { c.PluginPackages = &fakePkgSource{sha: "abc"} })
	resp, _ := chunkGet(t, clientFor(t, f.pin, nil), f.srv.URL, "abc", 0)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", resp.StatusCode)
	}
}

func TestFetchPluginChunk(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 1000)
	src := &fakePkgSource{sha: "abc", data: data}
	f := newMasterFixture(t, func(c *MasterConfig) { c.PluginPackages = src })
	sh, id, _ := startShipperWithFrames(t, f, func(Frame) {}, false)
	src.node = id

	b, size, err := sh.FetchPluginChunk(context.Background(), "abc", 400)
	if err != nil || size != 1000 || !bytes.Equal(b, data[400:]) {
		t.Fatalf("chunk len %d size %d err %v", len(b), size, err)
	}
	if _, _, err := sh.FetchPluginChunk(context.Background(), "nope", 0); err == nil {
		t.Fatal("fetch of an unoffered sha succeeded")
	}
}
