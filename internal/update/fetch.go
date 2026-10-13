package update

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

const maxSmallAsset = 1 << 20 // manifest, signatures, pointers

func readSmall(ctx context.Context, open func() (io.ReadCloser, error)) ([]byte, error) {
	rc, err := open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, maxSmallAsset+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxSmallAsset {
		return nil, fmt.Errorf("%w: asset larger than %d bytes", ErrMalformed, maxSmallAsset)
	}
	return b, nil
}

// FetchRelease downloads manifest.json and both signatures for version and verifies them.
func FetchRelease(ctx context.Context, src Source, keys KeySet, version string) (Manifest, []byte, error) {
	get := func(name string) ([]byte, error) {
		return readSmall(ctx, func() (io.ReadCloser, error) { return src.ReleaseAsset(ctx, version, name) })
	}
	mb, err := get("manifest.json")
	if err != nil {
		return Manifest{}, nil, fmt.Errorf("update: manifest for %s: %w", version, err)
	}
	cs, err := get("manifest.ci.sig")
	if err != nil {
		cs = nil
	}
	ms, err := get("manifest.maint.sig")
	if err != nil {
		ms = nil
	}
	m, err := VerifyRelease(keys, mb, cs, ms)
	if err != nil {
		return Manifest{}, nil, err
	}
	return m, mb, nil
}

// ErrNoPointer means the source answered but has no <channel>.json or no signature for it:
// together with ErrExpired, the signature of a withheld.
var ErrNoPointer = errors.New("update: channel pointer or its signature is missing")

// FetchLatest reads and verifies <channel>.json from the source's channel pointers.
func FetchLatest(ctx context.Context, src Source, keys KeySet, channel string, now time.Time) (Pointer, error) {
	get := func(name string) ([]byte, error) {
		b, err := readSmall(ctx, func() (io.ReadCloser, error) { return src.ChannelAsset(ctx, name) })
		if err != nil && (errors.Is(err, ErrNotFound) || errors.Is(err, fs.ErrNotExist)) {
			return nil, fmt.Errorf("%w: %s: %v", ErrNoPointer, name, err)
		}
		return b, err
	}
	pb, err := get(channel + ".json")
	if err != nil {
		return Pointer{}, err
	}
	sig, err := get(channel + ".json.sig")
	if err != nil {
		return Pointer{}, err
	}
	p, err := VerifyPointer(keys, pb, sig, now)
	if err != nil {
		return Pointer{}, err
	}
	if p.Channel != channel {
		return Pointer{}, fmt.Errorf("%w: pointer for %s, wanted %s", ErrWrongChannel, p.Channel, channel)
	}
	return p, nil
}

// FetchVerified streams one release file into dstDir, checking size and SHA-256 as it goes.
func FetchVerified(ctx context.Context, src Source, version string, f File, dstDir string, perm os.FileMode) (string, error) {
	rc, err := src.ReleaseAsset(ctx, version, f.Name)
	if err != nil {
		return "", fmt.Errorf("update: fetch %s: %w", f.Name, err)
	}
	defer rc.Close()
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", err
	}
	part := filepath.Join(dstDir, "."+f.Name+".part-"+hex.EncodeToString(rnd[:]))
	out, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		if !ok {
			out.Close()
			os.Remove(part)
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(rc, f.Size+1))
	if err != nil {
		return "", fmt.Errorf("update: fetch %s: %w", f.Name, err)
	}
	if n != f.Size {
		return "", fmt.Errorf("%w: %s is %d bytes, manifest says %d", ErrMalformed, f.Name, n, f.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != f.SHA256 {
		return "", fmt.Errorf("%w: %s sha256 %s, manifest says %s", ErrMalformed, f.Name, got, f.SHA256)
	}
	if err := out.Sync(); err != nil {
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(part, perm); err != nil {
		return "", err
	}
	final := filepath.Join(dstDir, f.Name)
	if err := os.Rename(part, final); err != nil {
		return "", err
	}
	ok = true
	if err := SyncDir(dstDir); err != nil {
		os.Remove(final)
		return "", err
	}
	return final, nil
}

// HashFile returns the hex SHA-256 of a file (used to re-check a staged file
// right before it is swapped in).
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
