package control

import (
	"bufio"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

func TestGrantAllows(t *testing.T) {
	if !FullAccess("ctl").Allows("Snapshot") || !FullAccess("ctl").Allows("Plugins.List") {
		t.Fatal("full access should allow core and Plugins methods")
	}
	g := Grant{PluginID: "p", Methods: map[string]bool{"Plugin.Log": true}}
	if !g.Allows("Plugin.Log") || g.Allows("Snapshot") {
		t.Fatal("plugin grant should allow only listed methods")
	}
}

func TestTokensRemoveRevokes(t *testing.T) {
	tk := NewTokens()
	tk.Set("aaa", Grant{Name: "n1", PluginID: "p"})
	tk.Set("bbb", Grant{Name: "n2", PluginID: "q"})
	if g, ok := tk.Authorize("aaa"); !ok || g.PluginID != "p" {
		t.Fatalf("authorize: %v %v", g, ok)
	}
	tk.Remove("aaa")
	if _, ok := tk.Authorize("aaa"); ok {
		t.Fatal("removed token still authorizes")
	}
	tk.RemoveName("n2")
	if _, ok := tk.Authorize("bbb"); ok {
		t.Fatal("RemoveName did not revoke")
	}
	if _, ok := tk.Authorize(""); ok {
		t.Fatal("empty token must not authorize")
	}
}

type pipeEnv struct {
	conn net.Conn
	r    *bufio.Reader
	done chan struct{}
}

func dialAuth(t *testing.T, api core.API, host plugin.Host, auth Authorizer, tok string) (*pipeEnv, bool) {
	t.Helper()
	server, client := net.Pipe()
	done := make(chan struct{})
	go func() {
		serveConn(api, host, server, auth)
		close(done)
	}()
	t.Cleanup(func() { client.Close() })
	_ = client.SetDeadline(time.Now().Add(10 * time.Second))
	if err := writeFrame(client, hello{Hello: helloMagic, Version: ProtocolVersion, Token: tok}); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(client)
	var h hello
	var resp response
	line, err := r.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(line, &h) == nil && h.Hello == helloMagic {
		return &pipeEnv{client, r, done}, true
	}
	_ = json.Unmarshal(line, &resp)
	if !strings.Contains(resp.Error, "invalid token") {
		t.Fatalf("unexpected hello reply %s", line)
	}
	return &pipeEnv{client, r, done}, false
}

func (e *pipeEnv) call(t *testing.T, id int, method string, params any, node string) response {
	t.Helper()
	raw, _ := json.Marshal(params)
	if err := writeFrame(e.conn, request{ID: id, Method: method, Params: raw, Node: node}); err != nil {
		t.Fatalf("write %s: %v", method, err)
	}
	var resp response
	if err := readFrame(e.r, &resp); err != nil {
		t.Fatalf("read %s: %v", method, err)
	}
	return resp
}

func TestUnknownTokenRejected(t *testing.T) {
	tk := NewTokens()
	tk.Set("good", FullAccess("ctl"))
	env, ok := dialAuth(t, &fakeAPI{}, nil, tk, "bad")
	if ok {
		t.Fatal("bad token accepted")
	}
	select {
	case <-env.done:
	case <-time.After(3 * time.Second):
		t.Fatal("connection not closed")
	}
}

func TestFullTokenKeepsFullAccess(t *testing.T) {
	api := &fakeAPI{}
	env, ok := dialAuth(t, api, nil, singleToken("tok"), "tok")
	if !ok {
		t.Fatal("rejected")
	}
	if r := env.call(t, 1, "Snapshot", struct{}{}, ""); !r.OK {
		t.Fatalf("Snapshot: %s", r.Error)
	}
	if r := env.call(t, 2, "Fleet.Status", struct{}{}, ""); strings.Contains(r.Error, "not permitted") {
		t.Fatalf("Fleet.Status: %s", r.Error)
	}
	r := env.call(t, 3, "Plugins.List", struct{}{}, "")
	if r.OK || !strings.Contains(r.Error, plugin.ErrUnavailable.Error()) {
		t.Fatalf("Plugins.List: ok=%v err=%s", r.OK, r.Error)
	}
	if r := env.call(t, 4, "Plugin.Log", plugin.LogEntry{}, ""); !strings.Contains(r.Error, "not permitted") {
		t.Fatalf("full token must not reach Plugin.*: %s", r.Error)
	}
}

func TestPluginTokenCannotRouteToNode(t *testing.T) {
	tk := NewTokens()
	tk.Set("pt", Grant{Name: "p", PluginID: "p", Methods: plugin.AllowedMethods(plugin.V1Permissions)})
	env, ok := dialAuth(t, &fakeAPI{}, &fakeHost{}, tk, "pt")
	if !ok {
		t.Fatal("rejected")
	}
	for _, node := range []string{"other", "abc"} {
		r := env.call(t, 1, plugin.MethodLog, plugin.LogEntry{Level: "info", Message: "x"}, node)
		if r.OK || !strings.Contains(r.Error, "not permitted") {
			t.Fatalf("node %q: ok=%v err=%s", node, r.OK, r.Error)
		}
	}
	for _, node := range []string{"", "self"} {
		if r := env.call(t, 2, plugin.MethodLog, plugin.LogEntry{Level: "info", Message: "x"}, node); !r.OK {
			t.Fatalf("node %q: %s", node, r.Error)
		}
	}
}

func TestPluginTokenOversizedRequestClosesConn(t *testing.T) {
	tk := NewTokens()
	tk.Set("pt", Grant{Name: "p", PluginID: "p", Methods: plugin.AllowedMethods(nil)})
	env, ok := dialAuth(t, &fakeAPI{}, &fakeHost{}, tk, "pt")
	if !ok {
		t.Fatal("rejected")
	}
	go func() {
		big := `{"id":1,"method":"Plugin.Log","params":{"message":"` + strings.Repeat("a", 2*plugin.MaxMessageBytes) + `"}}` + "\n"
		_, _ = env.conn.Write([]byte(big))
	}()
	select {
	case <-env.done:
	case <-time.After(5 * time.Second):
		t.Fatal("connection stayed open after oversized request")
	}
}

func goLiterals(t *testing.T, files ...string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	fset := token.NewFileSet()
	for _, f := range files {
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			cc, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, e := range cc.List {
				if bl, ok := e.(*ast.BasicLit); ok && bl.Kind == token.STRING {
					s, _ := strconv.Unquote(bl.Value)
					out[s] = true
				}
			}
			return true
		})
	}
	return out
}

func allKnownMethods() []string {
	set := map[string]bool{}
	for _, l := range [][]string{CoreMethods, fleetMethods, statusPageMethods, PluginsMethods} {
		for _, m := range l {
			set[m] = true
		}
	}
	for m := range plugin.MethodPermissions {
		set[m] = true
	}
	var out []string
	for m := range set {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

func TestMethodListsMatchDispatch(t *testing.T) {
	cases := goLiterals(t, "server.go", "plugins.go", "plugin_host.go")
	known := map[string]bool{}
	for _, m := range allKnownMethods() {
		known[m] = true
	}
	for c := range cases {
		if !known[c] {
			t.Errorf("dispatch handles %q but no method list names it", c)
		}
	}
	for _, l := range [][]string{CoreMethods, fleetMethods, statusPageMethods, PluginsMethods} {
		for _, m := range l {
			if m != "Subscribe" && !cases[m] {
				t.Errorf("method list names %q but no dispatch case handles it", m)
			}
		}
	}
}

func TestPluginTokenMethodMatrix(t *testing.T) {
	grants := map[string]Grant{
		"none":    {PluginID: "p", Methods: plugin.AllowedMethods(nil)},
		"publish": {PluginID: "p", Methods: plugin.AllowedMethods([]plugin.Permission{plugin.PermMetricsPublish, plugin.PermAlertsRaise})},
		"all":     {PluginID: "p", Methods: plugin.AllowedMethods(plugin.V1Permissions)},
	}
	for name, g := range grants {
		tk := NewTokens()
		tk.Set("tok", g)
		for _, m := range allKnownMethods() {
			env, ok := dialAuth(t, &fakeAPI{}, &fakeHost{}, tk, "tok")
			if !ok {
				t.Fatal("rejected")
			}
			r := env.call(t, 1, m, map[string]any{}, "")
			denied := strings.Contains(r.Error, "not permitted")
			if denied == g.Methods[m] {
				t.Errorf("%s/%s: allowed=%v but error=%q", name, m, g.Methods[m], r.Error)
			}
			env.conn.Close()
		}
	}
}
