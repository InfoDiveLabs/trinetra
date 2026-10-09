package trinetra

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
)

func jsonIndent(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }

// errNotExist is the shared "file not found" sentinel used by fake FileSource
// implementations in tests.
var errNotExist = os.ErrNotExist

// reloadDaemon sends SIGHUP to a running daemon via its pidfile. Best-effort.
func reloadDaemon() {
	b, err := os.ReadFile(pidFile())
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(string(trimSpace(b)))
	if err != nil {
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Signal(sighup)
	}
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == ' ' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

func pidFile() string { return stateDir + "/trinetra.pid" }

func sortStrings(s []string) { sort.Strings(s) }

// writeFileAtomic replaces path with b via a temp file in the same directory
// and a rename. The temp name is unique per call, so concurrent writers of
// the same path (e.g. two live updates from one node) cannot rename each
// other's half-written file or find it already gone.
func writeFileAtomic(path string, b []byte, perm os.FileMode) error {
	return writeFileAtomicImpl(path, b, perm, false)
}

// writeFileAtomicSynced is writeFileAtomic plus an fsync of the temp file's
// contents before the rename, for callers that need the write to survive a
// crash immediately after it returns (private keys, silences/managed/
// routing config, ingest state). It replaces the old, independent
// writeFileSynced helper, which used a fixed (non-unique) temp filename and
// reintroduced the exact concurrent-rename race writeFileAtomic was fixed
// for.
func writeFileAtomicSynced(path string, b []byte, perm os.FileMode) error {
	return writeFileAtomicImpl(path, b, perm, true)
}

func writeFileAtomicImpl(path string, b []byte, perm os.FileMode, sync bool) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(b)
	if werr == nil && sync {
		werr = f.Sync()
	}
	cerr := f.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp, perm)
	}
	if werr == nil {
		werr = os.Rename(tmp, path)
	}
	if werr != nil {
		_ = os.Remove(tmp)
	}
	return werr
}
