package update

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
)

// SyncDir fsyncs a directory, making a rename or create inside it durable.
var SyncDir = func(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// writeTemp streams r into a fresh O_EXCL temp file next to path, fsyncs and closes it, and
// sets perm.
func writeTemp(path string, r io.Reader, perm os.FileMode) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	fail := func(err error) (string, error) {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	if _, err := io.Copy(f, r); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return tmp, nil
}

// replaceWith renames tmp over path and fsyncs path's directory.
func replaceWith(tmp, path string) error {
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return SyncDir(filepath.Dir(path))
}

// CopyFile copies src onto dst durably and atomically: a random same-directory temp file
// (O_EXCL), write, fsync, close, chmod, rename over dst.
func CopyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := writeTemp(dst, in, perm)
	if err != nil {
		return err
	}
	return replaceWith(tmp, dst)
}

// WriteFileAtomic writes data to path with the same guarantees as CopyFile.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := writeTemp(path, bytes.NewReader(data), perm)
	if err != nil {
		return err
	}
	return replaceWith(tmp, path)
}
