package fleet

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRegistryAddGetListPersist(t *testing.T) {
	p := filepath.Join(t.TempDir(), "registry.json")
	r, err := OpenRegistry(p)
	if err != nil {
		t.Fatal(err)
	}
	id1, _ := NewNodeID()
	id2, _ := NewNodeID()
	if len(id1) != 32 || id1 == id2 {
		t.Fatalf("ids %q %q", id1, id2)
	}
	if err := r.Add(Node{ID: id1, Name: "web-2", Tags: []string{"web"}}); err != nil {
		t.Fatal(err)
	}
	if err := r.Add(Node{ID: id2, Name: "db-1"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Add(Node{ID: id1, Name: "dup"}); err == nil {
		t.Fatal("duplicate id accepted")
	}
	assertMode(t, p, 0o600)
	r2, err := OpenRegistry(p)
	if err != nil {
		t.Fatal(err)
	}
	l := r2.List()
	if len(l) != 2 || l[0].Name != "db-1" || l[1].Name != "web-2" {
		t.Fatalf("list = %+v", l)
	}
}

func TestRegistryTouchFlushAndRevoke(t *testing.T) {
	p := filepath.Join(t.TempDir(), "registry.json")
	r, _ := OpenRegistry(p)
	id, _ := NewNodeID()
	_ = r.Add(Node{ID: id, Name: "n"})
	r.Touch(id, 500, "10.0.0.2:5555", "v0.5.0")
	if n, _ := r.Get(id); n.LastSeen != 500 || n.Version != "v0.5.0" {
		t.Fatalf("touch not visible in memory: %+v", n)
	}
	if err := r.FlushIfDirty(); err != nil {
		t.Fatal(err)
	}
	r2, _ := OpenRegistry(p)
	if n, _ := r2.Get(id); n.LastSeen != 500 {
		t.Fatalf("touch not flushed: %+v", n)
	}
	if r.IsRevoked(id) {
		t.Fatal("fresh node revoked")
	}
	if !r.IsRevoked("unknown") {
		t.Fatal("unknown node must count as revoked")
	}
	if err := r.Update(id, func(n *Node) error { n.Revoked = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if !r.IsRevoked(id) {
		t.Fatal("revoke not applied")
	}
}

// TestRegistryAddDedupesNameCaseInsensitive is the review round-2 item (b)
// regression test: a join whose requested name collides (case-insensitively)
// with an existing node's is registered as "<name>-2", "-3", ... instead of
// silently sharing the name.
func TestRegistryAddDedupesNameCaseInsensitive(t *testing.T) {
	p := filepath.Join(t.TempDir(), "registry.json")
	r, err := OpenRegistry(p)
	if err != nil {
		t.Fatal(err)
	}
	id1, _ := NewNodeID()
	id2, _ := NewNodeID()
	id3, _ := NewNodeID()
	if err := r.Add(Node{ID: id1, Name: "Web1"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Add(Node{ID: id2, Name: "web1"}); err != nil { // case-insensitive collision
		t.Fatal(err)
	}
	if err := r.Add(Node{ID: id3, Name: "WEB1"}); err != nil { // collides with both
		t.Fatal(err)
	}
	n1, _ := r.Get(id1)
	n2, _ := r.Get(id2)
	n3, _ := r.Get(id3)
	if n1.Name != "Web1" {
		t.Fatalf("first join name = %q, want unchanged Web1", n1.Name)
	}
	if n2.Name != "web1-2" {
		t.Fatalf("second join name = %q, want web1-2", n2.Name)
	}
	if n3.Name != "WEB1-3" {
		t.Fatalf("third join name = %q, want WEB1-3", n3.Name)
	}

	// A registry unrelated to these names is untouched by the dedup logic.
	id4, _ := NewNodeID()
	if err := r.Add(Node{ID: id4, Name: "db1"}); err != nil {
		t.Fatal(err)
	}
	if n4, _ := r.Get(id4); n4.Name != "db1" {
		t.Fatalf("unrelated name = %q, want unchanged db1", n4.Name)
	}
}

func TestRegistryNameConflict(t *testing.T) {
	p := filepath.Join(t.TempDir(), "registry.json")
	r, _ := OpenRegistry(p)
	id1, _ := NewNodeID()
	id2, _ := NewNodeID()
	_ = r.Add(Node{ID: id1, Name: "web1"})
	_ = r.Add(Node{ID: id2, Name: "db1"})

	if conflict, ok := r.NameConflict("WEB1", id2); !ok || conflict.ID != id1 {
		t.Fatalf("NameConflict(WEB1, id2) = %+v, %v, want id1's node", conflict, ok)
	}
	if _, ok := r.NameConflict("web1", id1); ok {
		t.Fatal("a node's own current name must not conflict with itself")
	}
	if _, ok := r.NameConflict("nobody-has-this", id1); ok {
		t.Fatal("an unused name must not conflict")
	}
}

func TestShortNodeID(t *testing.T) {
	if got := ShortNodeID("abcdefgh12345678"); got != "abcdefgh" {
		t.Fatalf("ShortNodeID = %q, want abcdefgh", got)
	}
	if got := ShortNodeID("short"); got != "short" {
		t.Fatalf("ShortNodeID(short) = %q, want unchanged", got)
	}
}

func TestDuplicateNames(t *testing.T) {
	nodes := []Node{
		{ID: "aaaaaaaaaaaa", Name: "web1"},
		{ID: "bbbbbbbbbbbb", Name: "Web1"},
		{ID: "cccccccccccc", Name: "db1"},
	}
	got := DuplicateNames(nodes)
	if len(got) != 1 || !strings.Contains(got[0], "web1") || !strings.Contains(got[0], "aaaaaaaa") || !strings.Contains(got[0], "bbbbbbbb") {
		t.Fatalf("DuplicateNames = %+v, want one entry for web1/Web1 naming both short ids", got)
	}
	if got := DuplicateNames([]Node{{ID: "x", Name: "unique"}}); len(got) != 0 {
		t.Fatalf("DuplicateNames with no collisions = %+v, want none", got)
	}
}

func TestRegistryDelete(t *testing.T) {
	p := filepath.Join(t.TempDir(), "registry.json")
	r, _ := OpenRegistry(p)
	id, _ := NewNodeID()
	_ = r.Add(Node{ID: id, Name: "n"})
	if err := r.Delete(id); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Get(id); ok {
		t.Fatal("deleted node still listed")
	}
	if r2, _ := OpenRegistry(p); len(r2.List()) != 0 {
		t.Fatal("delete not persisted")
	}
	if err := r.Delete(id); err == nil {
		t.Fatal("deleting an unknown node succeeded")
	}
}
