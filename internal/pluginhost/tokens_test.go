package pluginhost

import (
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/control"
	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

func TestRegisterMintsScopedGrant(t *testing.T) {
	tk := control.NewTokens()
	r := NewRegistrar(tk)
	tok, err := r.Register("alpha", []plugin.Permission{plugin.PermMetricsPublish}, false)
	if err != nil || len(tok) != 64 {
		t.Fatalf("tok=%q err=%v", tok, err)
	}
	g, ok := tk.Authorize(tok)
	if !ok || g.PluginID != "alpha" || g.Name != "plugin:alpha" || g.Full {
		t.Fatalf("grant %+v ok=%v", g, ok)
	}
	if !g.Allows(plugin.MethodPublishSeries) || g.Allows(plugin.MethodSnapshot) || !g.Allows(plugin.MethodHello) {
		t.Fatalf("methods %v", g.Methods)
	}
	perms, dev, ok := r.Granted("alpha")
	if !ok || dev || len(perms) != 1 {
		t.Fatalf("granted %v %v %v", perms, dev, ok)
	}
	if got, _ := r.Token("alpha"); got != tok {
		t.Fatal("token mismatch")
	}
}

func TestReRegisterRevokesOldToken(t *testing.T) {
	tk := control.NewTokens()
	r := NewRegistrar(tk)
	t1, _ := r.Register("alpha", nil, false)
	t2, _ := r.Register("alpha", nil, true)
	if t1 == t2 {
		t.Fatal("token reused")
	}
	if _, ok := tk.Authorize(t1); ok {
		t.Fatal("old token still valid")
	}
	if _, ok := tk.Authorize(t2); !ok {
		t.Fatal("new token invalid")
	}
	if _, dev, _ := r.Granted("alpha"); !dev {
		t.Fatal("dev flag lost")
	}
}

func TestUnregisterRevokes(t *testing.T) {
	tk := control.NewTokens()
	r := NewRegistrar(tk)
	tok, _ := r.Register("alpha", nil, false)
	r.Unregister("alpha")
	if _, ok := tk.Authorize(tok); ok {
		t.Fatal("token valid after unregister")
	}
	if _, _, ok := r.Granted("alpha"); ok {
		t.Fatal("still granted")
	}
	r.Unregister("alpha")
}
