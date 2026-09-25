package trinetra

import (
	"errors"
	"testing"
)

func TestParseDockerPS(t *testing.T) {
	s := "web\trunning\tUp 3 hours\n" +
		"db\texited\tExited (0) 2 hours ago\n"
	cs := parseDockerPS(s)
	if len(cs) != 2 || cs[0].Name != "web" || cs[0].State != "running" {
		t.Fatalf("containers = %+v", cs)
	}
	if cs[1].State != "exited" {
		t.Fatalf("db state = %q", cs[1].State)
	}
}

// fakeExec returns canned output per command name.
type fakeExec struct {
	fn func(name string, args ...string) ([]byte, error)
}

func (f fakeExec) Run(name string, args ...string) ([]byte, error) { return f.fn(name, args...) }

func TestParseDockerStats(t *testing.T) {
	s := "web\t11.20%\t512MiB / 16GiB\t2.1MB / 0.4MB\n" +
		"db\t0.50%\t1.5GiB / 16GiB\t500kB / 100kB\n" +
		"this line is garbage\n" +
		"cache\tnot-a-percent\t10MiB / 1GiB\t1MB / 1MB\n"
	cs := parseDockerStats(s)
	if len(cs) != 2 {
		t.Fatalf("parseDockerStats returned %d entries (%+v), want 2 (malformed lines skipped)", len(cs), cs)
	}

	web := cs[0]
	if web.Name != "web" {
		t.Fatalf("cs[0].Name = %q, want web", web.Name)
	}
	if web.CPUPct != 11.2 {
		t.Errorf("web.CPUPct = %v, want 11.2", web.CPUPct)
	}
	if web.MemMiB != 512 {
		t.Errorf("web.MemMiB = %v, want 512", web.MemMiB)
	}
	if web.NetRxMB != 2.1 {
		t.Errorf("web.NetRxMB = %v, want 2.1", web.NetRxMB)
	}
	if web.NetTxMB != 0.4 {
		t.Errorf("web.NetTxMB = %v, want 0.4", web.NetTxMB)
	}

	db := cs[1]
	if db.Name != "db" {
		t.Fatalf("cs[1].Name = %q, want db", db.Name)
	}
	if db.CPUPct != 0.5 {
		t.Errorf("db.CPUPct = %v, want 0.5", db.CPUPct)
	}
	// 1.5GiB -> MiB conversion.
	if db.MemMiB != 1536 {
		t.Errorf("db.MemMiB = %v, want 1536 (1.5GiB->MiB)", db.MemMiB)
	}
	// 500kB / 100kB -> MB conversion (decimal, 1000-based).
	if db.NetRxMB != 0.5 {
		t.Errorf("db.NetRxMB = %v, want 0.5 (500kB->MB)", db.NetRxMB)
	}
	if db.NetTxMB != 0.1 {
		t.Errorf("db.NetTxMB = %v, want 0.1 (100kB->MB)", db.NetTxMB)
	}
}

func TestParseDockerStatsEmpty(t *testing.T) {
	if cs := parseDockerStats(""); len(cs) != 0 {
		t.Fatalf("parseDockerStats(\"\") = %+v, want empty", cs)
	}
}

// fakeExecMulti differentiates docker ps vs docker stats by args[0], since
// dockerAccess.stats and dockerAccess.list both dispatch through "docker"/
// "sudo docker" with a different subcommand as the first arg.
func TestDockerAccessStats(t *testing.T) {
	x := fakeExec{fn: func(name string, args ...string) ([]byte, error) {
		if name == "docker" && len(args) > 0 && args[0] == "stats" {
			return []byte("web\t5.00%\t100MiB / 1GiB\t1MB / 1MB\n"), nil
		}
		return nil, errors.New("unexpected call " + name)
	}}
	a := dockerAccess{available: true, method: "socket"}
	cs, err := a.stats(x)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].Name != "web" || cs[0].CPUPct != 5 {
		t.Fatalf("stats = %+v", cs)
	}
}

func TestDockerAccessStatsUsesSudo(t *testing.T) {
	x := fakeExec{fn: func(name string, args ...string) ([]byte, error) {
		if name == "sudo" && len(args) > 1 && args[0] == "docker" && args[1] == "stats" {
			return []byte("db\t1.00%\t50MiB / 1GiB\t1MB / 1MB\n"), nil
		}
		return nil, errors.New("unexpected call " + name)
	}}
	a := dockerAccess{available: true, sudo: true, method: "sudo"}
	cs, err := a.stats(x)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].Name != "db" {
		t.Fatalf("stats = %+v", cs)
	}
}

func TestProbeDockerFallsBackToSudo(t *testing.T) {
	x := fakeExec{fn: func(name string, args ...string) ([]byte, error) {
		if name == "docker" {
			return nil, errors.New("permission denied on /var/run/docker.sock")
		}
		if name == "sudo" && len(args) > 0 && args[0] == "docker" {
			return []byte("web\trunning\tUp\n"), nil
		}
		return nil, errors.New("unexpected")
	}}
	// socket not present in fake fs
	fs := fakeFS{files: map[string]string{}}
	a := probeDocker(x, fs)
	if !a.available || !a.sudo {
		t.Fatalf("expected sudo fallback, got %+v", a)
	}
}
