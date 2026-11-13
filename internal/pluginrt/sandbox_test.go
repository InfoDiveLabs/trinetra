package pluginrt

import (
	"flag"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files")

func golden(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != got {
		t.Fatalf("%s differs:\n%s", name, got)
	}
}

func dropIn(perms ...plugin.Permission) string {
	return RenderDropIn("demo", "/usr/local/lib/trinetra/plugins/demo/1.0.0/bin/linux-amd64/trinetra-plugin-demo",
		"/run/trinetra-plugins/host.sock", "/var/lib/trinetra/plugins/demo/token", perms, DefaultLimits)
}

func TestRenderTemplateUnitGolden(t *testing.T) {
	golden(t, "template_unit.golden", RenderTemplateUnit())
}

func TestRenderDropInNoNetwork(t *testing.T) {
	golden(t, "dropin_nonet.golden", dropIn(plugin.PermMetricsRead))
}

func TestRenderDropInOutbound(t *testing.T) {
	golden(t, "dropin_outbound.golden", dropIn(plugin.PermNetOutbound))
}

func TestRenderDropInListen(t *testing.T) {
	golden(t, "dropin_listen.golden", dropIn(plugin.PermNetListen))
}

func TestRenderDropInRejectsBadID(t *testing.T) {
	for _, id := range []string{"../x", "A", "", "a b", "x\nExecStart=/bin/sh"} {
		if got := RenderDropIn(id, "/x", "/h", "/t", nil, DefaultLimits); got != "" {
			t.Errorf("id %q rendered %q", id, got)
		}
	}
}

func TestDropInPath(t *testing.T) {
	if DropInPath("demo") != "/etc/systemd/system/trinetra-plugin@demo.service.d/50-trinetra.conf" {
		t.Fatal(DropInPath("demo"))
	}
}

func TestRlimits(t *testing.T) {
	rl, err := Rlimits(DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int]Rlimit{}
	for _, r := range rl {
		got[r.Resource] = r
	}
	if got[syscall.RLIMIT_DATA].Cur != 256<<20 || got[rlimitNproc].Cur != SharedNprocCap ||
		got[syscall.RLIMIT_NOFILE].Cur != 1024 || got[syscall.RLIMIT_CORE].Max != 0 || len(got) != 4 {
		t.Fatalf("rlimits = %+v", rl)
	}
	if _, ok := got[syscall.RLIMIT_AS]; ok {
		t.Fatalf("rlimits = %+v", rl)
	}
	if _, err := Rlimits(Limits{MemoryMax: "lots", TasksMax: 1}); err == nil {
		t.Fatal("expected size error")
	}
}
