package trinetra

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/fleet"
)

func sliceFetcher(data []byte, chunk int, offs *[]int64) chunkFetcher {
	return func(_ context.Context, _ string, off int64) ([]byte, int64, error) {
		*offs = append(*offs, off)
		end := min(int(off)+chunk, len(data))
		return data[off:end], int64(len(data)), nil
	}
}

func shaOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestDownloadResumesFromPartialFile(t *testing.T) {
	data := make([]byte, 1000)
	for i := range data {
		data[i] = byte(i)
	}
	sha := shaOf(data)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, sha+".part"), data[:500], 0o600); err != nil {
		t.Fatal(err)
	}
	var offs []int64
	var last int64
	path, err := downloadPackage(context.Background(), sliceFetcher(data, 200, &offs), dir, sha, 1000, func(r int64) { last = r })
	if err != nil {
		t.Fatal(err)
	}
	if len(offs) == 0 || offs[0] != 500 {
		t.Fatalf("fetch offsets = %v, want first 500", offs)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(data) || path != filepath.Join(dir, sha+".tar.gz") || last != 1000 {
		t.Fatalf("path %s last %d match=%v", path, last, string(got) == string(data))
	}
	if _, err := os.Stat(filepath.Join(dir, sha+".part")); !os.IsNotExist(err) {
		t.Fatal("part file left behind")
	}
}

func TestDownloadRejectsHashMismatch(t *testing.T) {
	data := []byte("hello world, this is a package")
	sha := shaOf([]byte("something else"))
	dir := t.TempDir()
	var offs []int64
	if _, err := downloadPackage(context.Background(), sliceFetcher(data, 1<<10, &offs), dir, sha, int64(len(data)), nil); err == nil {
		t.Fatal("hash mismatch accepted")
	}
	for _, ext := range []string{".part", ".tar.gz"} {
		if _, err := os.Stat(filepath.Join(dir, sha+ext)); !os.IsNotExist(err) {
			t.Fatalf("%s left behind", ext)
		}
	}
}

func TestDownloadRejectsOverSize(t *testing.T) {
	data := []byte("0123456789")
	sha := shaOf(data)
	dir := t.TempDir()
	var offs []int64
	if _, err := downloadPackage(context.Background(), sliceFetcher(data, 100, &offs), dir, sha, 5, nil); err == nil {
		t.Fatal("chunk beyond the offered size accepted")
	}
	if _, err := downloadPackage(context.Background(), sliceFetcher(data, 100, &offs), dir, sha, fleet.MaxPluginPackage+1, nil); err == nil {
		t.Fatal("offer over the package cap accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, sha+".part")); !os.IsNotExist(err) {
		t.Fatal("part file left behind")
	}
}

func TestDownloadRejectsSizeChange(t *testing.T) {
	data := []byte("0123456789")
	sha := shaOf(data)
	dir := t.TempDir()
	fetch := func(context.Context, string, int64) ([]byte, int64, error) { return data[:4], 11, nil }
	if _, err := downloadPackage(context.Background(), fetch, dir, sha, 10, nil); err == nil {
		t.Fatal("size change accepted")
	}
	fetchErr := func(context.Context, string, int64) ([]byte, int64, error) { return nil, 0, errors.New("net down") }
	if _, err := downloadPackage(context.Background(), fetchErr, dir, sha, 10, nil); err == nil {
		t.Fatal("fetch error swallowed")
	}
}

func TestDownloadRejectsBadDigest(t *testing.T) {
	var offs []int64
	if _, err := downloadPackage(context.Background(), sliceFetcher([]byte("x"), 1, &offs), t.TempDir(), "../../etc/passwd", 1, nil); err == nil {
		t.Fatal("non-hex digest accepted")
	}
}
