package fleet

import (
	"path/filepath"
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
