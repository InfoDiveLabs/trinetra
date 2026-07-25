package serverwatch

import (
	"encoding/json"
	"os"
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

func pidFile() string { return stateDir + "/serverwatch.pid" }

func sortStrings(s []string) { sort.Strings(s) }

func writeFileAtomic(path string, b []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
