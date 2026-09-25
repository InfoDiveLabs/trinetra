package serverwatch

import (
	"bytes"
	"crypto/tls"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"serverwatch/internal/config"
	"serverwatch/internal/fleet"
)

func fleetCLIEnv(t *testing.T) (dir string, out, errb *bytes.Buffer) {
	t.Helper()
	dir = t.TempDir()
	prevState, prevCfg, prevOut, prevErr := stateDir, cfgPath, stdout, stderr
	stateDir, cfgPath = dir, filepath.Join(dir, "config.json")
	out, errb = &bytes.Buffer{}, &bytes.Buffer{}
	stdout, stderr = out, errb
	t.Setenv("RUNTIME_DIRECTORY", t.TempDir())
	t.Cleanup(func() { stateDir, cfgPath, stdout, stderr = prevState, prevCfg, prevOut, prevErr })
	return dir, out, errb
}

func loadTestCfg(t *testing.T) *config.Config {
	c, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestFleetInitWritesPKIAndConfig(t *testing.T) {
	dir, out, errb := fleetCLIEnv(t)
	if code := Main([]string{"fleet", "init", "--address", "127.0.0.1,mon.local", "--port", "9555"}); code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	c := loadTestCfg(t)
	if c.FleetRole() != config.RoleMaster || c.Fleet.Address != "127.0.0.1,mon.local" || c.FleetListen() != ":9555" {
		t.Fatalf("fleet cfg = %+v", c.Fleet)
	}
	for _, f := range []string{"ca.crt", "ca.key", "server.crt", "server.key"} {
		if _, err := os.Stat(filepath.Join(dir, "fleet", "pki", f)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	if fi, _ := os.Stat(filepath.Join(dir, "fleet", "pki", "ca.key")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("ca.key mode %v", fi.Mode().Perm())
	}
	if !strings.Contains(out.String(), "sha256:") || !strings.Contains(out.String(), "systemctl restart") {
		t.Fatalf("output = %s", out)
	}
	if code := Main([]string{"fleet", "init", "--address", "x"}); code == 0 {
		t.Fatal("second init on a master succeeded")
	}
}

func TestFleetInitRequiresAddress(t *testing.T) {
	fleetCLIEnv(t)
	if code := Main([]string{"fleet", "init"}); code == 0 {
		t.Fatal("init without --address succeeded")
	}
}

func testMasterForCLI(t *testing.T) (code string) {
	t.Helper()
	mdir := t.TempDir()
	if err := fleetInitPKI(mdir, []string{"127.0.0.1"}, "t", time.Now()); err != nil {
		t.Fatal(err)
	}
	pki := fleetPKIDir(mdir)
	ca, _ := fleet.LoadCA(filepath.Join(pki, "ca.crt"), filepath.Join(pki, "ca.key"))
	leaf, _ := tls.LoadX509KeyPair(filepath.Join(pki, "server.crt"), filepath.Join(pki, "server.key"))
	reg, _ := fleet.OpenRegistry(filepath.Join(mdir, "fleet", "registry.json"))
	toks, _ := fleet.OpenTokens(filepath.Join(mdir, "fleet", "tokens.json"))
	m := fleet.NewMaster(fleet.MasterConfig{CA: ca, Leaf: leaf, Registry: reg, Tokens: toks, Sink: newReplicaSink(filepath.Join(mdir, "nodes"), StoreOptions{})})
	srv := httptest.NewUnstartedServer(m.Handler())
	srv.TLS = fleet.ServerTLS(leaf, ca.Cert)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	plain, _, _ := toks.Create(time.Hour, 1, nil, "t", time.Now())
	return fleet.EncodeJoin(fleet.JoinInfo{URL: srv.URL, Token: plain, Pin: fleet.SPKIPin(ca.Cert)})
}

func TestFleetJoinAndLeave(t *testing.T) {
	dir, _, errb := fleetCLIEnv(t)
	code := testMasterForCLI(t)
	if rc := Main([]string{"fleet", "join", code, "--name", "box-1"}); rc != 0 {
		t.Fatalf("join exit %d: %s", rc, errb)
	}
	c := loadTestCfg(t)
	if c.FleetRole() != config.RoleChild || c.Fleet.NodeID == "" || c.Fleet.CAPin == "" || c.Fleet.MasterURL == "" {
		t.Fatalf("fleet cfg = %+v", c.Fleet)
	}
	if _, err := os.Stat(filepath.Join(dir, "fleet-child", "node.key")); err != nil {
		t.Fatal(err)
	}
	if rc := Main([]string{"fleet", "join", code}); rc == 0 {
		t.Fatal("join while already a child succeeded")
	}
	if rc := Main([]string{"fleet", "leave", "--purge"}); rc != 0 {
		t.Fatalf("leave exit %d: %s", rc, errb)
	}
	if c := loadTestCfg(t); c.FleetRole() != config.RoleSolo || c.Fleet.NodeID != "" {
		t.Fatalf("after leave = %+v", c.Fleet)
	}
	if _, err := os.Stat(filepath.Join(dir, "fleet-child")); !os.IsNotExist(err) {
		t.Fatal("purge kept fleet-child")
	}
}

func TestFleetDaemonCommandsExplainWhenDaemonDown(t *testing.T) {
	_, _, errb := fleetCLIEnv(t)
	if rc := Main([]string{"fleet", "nodes"}); rc != 1 {
		t.Fatalf("exit %d", rc)
	}
	if !strings.Contains(errb.String(), "not reachable") {
		t.Fatalf("stderr = %s", errb)
	}
}

func TestFleetUnknownSubcommand(t *testing.T) {
	fleetCLIEnv(t)
	if rc := Main([]string{"fleet", "bogus"}); rc != 2 {
		t.Fatalf("exit %d, want 2", rc)
	}
}
