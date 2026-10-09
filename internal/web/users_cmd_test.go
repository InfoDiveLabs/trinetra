package web

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

func TestEnrollURL(t *testing.T) {
	for _, tc := range []struct {
		origin, listen, want string
		note                 bool
	}{
		{"https://status.example.com", "127.0.0.1:8088", "https://status.example.com/enroll?token=T", false},
		{"https://host:8443/", "", "https://host:8443/enroll?token=T", false},
		{"", "127.0.0.1:8088", "http://127.0.0.1:8088/enroll?token=T", true},
	} {
		c := config.Default()
		c.Web.Origin, c.Web.Listen = tc.origin, tc.listen
		got, note := enrollURL(c, "T")
		if got != tc.want || (note != "") != tc.note {
			t.Errorf("%q/%q: %q %q", tc.origin, tc.listen, got, note)
		}
	}
}

func runUsers(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	c := config.Default()
	c.Web.Origin = "https://ops.example.com"
	code := RunUsersCommand(args, dir, c, "cli:root", &out, &errb)
	return code, out.String(), errb.String()
}

func TestUsersInviteListSetRoleRemove(t *testing.T) {
	dir := t.TempDir()
	code, out, _ := runUsers(t, dir, "invite", "--role", "admin")
	if code != 0 || !strings.Contains(out, "https://ops.example.com/enroll?token=") {
		t.Fatalf("invite: %d %s", code, out)
	}
	tok := strings.TrimPrefix(strings.SplitN(out, "\n", 2)[0], "https://ops.example.com/enroll?token=")
	if role, _, err := resolveEnrollRole(newTokenStore(dir), newUserStore(dir), tok, false); err != nil || role != RoleAdmin {
		t.Fatalf("printed token does not redeem as admin: %v %v", role, err)
	}
	if code, _, _ := runUsers(t, dir, "invite", "--role", "boss"); code != 2 {
		t.Errorf("bad role accepted: %d", code)
	}
	store := newUserStore(dir)
	for _, u := range []*User{{ID: "a1", Name: "alice", Role: RoleAdmin, Created: 1}, {ID: "b1", Name: "bob", Role: RoleViewer, Created: 1}} {
		if err := store.Put(u); err != nil {
			t.Fatal(err)
		}
	}
	code, out, _ = runUsers(t, dir, "list", "--json")
	var rows []map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 2 {
		t.Fatalf("list: %d %s", code, out)
	}
	if code, _, _ := runUsers(t, dir, "set-role", "bob", "responder"); code != 0 {
		t.Fatal("set-role failed")
	}
	if u, _ := store.ByName("bob"); u.Role != RoleResponder {
		t.Fatalf("role = %s", u.Role)
	}
	if code, _, errs := runUsers(t, dir, "remove", "alice"); code != 1 || !strings.Contains(errs, "last admin") {
		t.Fatalf("removed last admin: %d %s", code, errs)
	}
	if code, _, _ := runUsers(t, dir, "remove", "bob"); code != 0 {
		t.Fatal("remove bob failed")
	}
	if code, _, _ := runUsers(t, dir, "remove", "nobody"); code != 1 {
		t.Error("removing unknown user succeeded")
	}
	b, _ := os.ReadFile(auditLogPath(dir))
	var actions []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var r AuditRecord
		if json.Unmarshal([]byte(line), &r) == nil {
			actions = append(actions, r.Action+"/"+r.User)
		}
	}
	got := strings.Join(actions, " ")
	for _, want := range []string{"user.invite/cli:root", "user.role/cli:root", "user.remove/cli:root"} {
		if !strings.Contains(got, want) {
			t.Errorf("audit missing %s: %s", want, got)
		}
	}
}
