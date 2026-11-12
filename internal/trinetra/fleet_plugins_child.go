package trinetra

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/InfoDiveLabs/trinetra/internal/fleet"
)

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type chunkFetcher func(ctx context.Context, sha string, off int64) ([]byte, int64, error)

// downloadPackage resumes <dir>/<sha>.part from its current size, verifies sha256 at the end, and returns the final path (<dir>/<sha>.tar.gz).
func downloadPackage(ctx context.Context, fetch chunkFetcher, dir, sha string, size int64, progress func(received int64)) (string, error) {
	sha = strings.ToLower(sha)
	if !sha256Pattern.MatchString(sha) {
		return "", errors.New("invalid package digest")
	}
	if size <= 0 || size > fleet.MaxPluginPackage {
		return "", fmt.Errorf("package size %d is outside the allowed range", size)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	part := filepath.Join(dir, sha+".part")
	final := filepath.Join(dir, sha+".tar.gz")
	f, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fail := func(err error) (string, error) {
		f.Close()
		os.Remove(part)
		return "", err
	}
	off, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return "", err
	}
	if off > size {
		if err := f.Truncate(0); err != nil {
			return fail(err)
		}
		off = 0
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return "", err
	}
	if progress != nil {
		progress(off)
	}
	for off < size {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		chunk, total, err := fetch(ctx, sha, off)
		if err != nil {
			return "", err
		}
		if total != size {
			return fail(fmt.Errorf("package size changed from %d to %d during transfer", size, total))
		}
		if len(chunk) == 0 {
			return "", errors.New("master ended the transfer early")
		}
		if off+int64(len(chunk)) > size {
			return fail(errors.New("package is larger than offered"))
		}
		if _, err := f.Write(chunk); err != nil {
			return "", err
		}
		off += int64(len(chunk))
		if progress != nil {
			progress(off)
		}
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	if hex.EncodeToString(h.Sum(nil)) != sha {
		return fail(errors.New("package digest mismatch"))
	}
	if err := os.Rename(part, final); err != nil {
		return "", err
	}
	return final, nil
}
