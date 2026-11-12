package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runCLI(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestRunArgumentRouting(t *testing.T) {
	if code, _, e := runCLI(); code != 2 || !strings.Contains(e, "usage") {
		t.Fatalf("no args: %d %q", code, e)
	}
	if code, _, e := runCLI("bogus"); code != 2 || !strings.Contains(e, "unknown command") {
		t.Fatalf("unknown: %d %q", code, e)
	}
	for name := range commands {
		if code, _, e := runCLI(name); code != 1 || !strings.Contains(e, "required") {
			t.Errorf("%s with no flags: %d %q", name, code, e)
		}
	}
}

func TestKeygenSignVerifyCheck(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	code, out, e := runCLI("keygen", "-out", key)
	if code != 0 || !strings.HasPrefix(out, "public: ") {
		t.Fatalf("keygen: %d %q %q", code, out, e)
	}
	pub := strings.TrimSpace(strings.Split(strings.Split(out, "\n")[0], "public: ")[1])
	msg := filepath.Join(dir, "m.json")
	if err := os.WriteFile(msg, []byte(`{"a":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, e := runCLI("sign", "-key", key, "-kind", "manifest", "-in", msg, "-out", msg+".sig"); code != 0 {
		t.Fatalf("sign: %d %q", code, e)
	}
	if code, out, e := runCLI("verify", "-pubkey", pub, "-kind", "manifest", "-in", msg, "-sig", msg+".sig"); code != 0 || strings.TrimSpace(out) != "ok" {
		t.Fatalf("verify: %d %q %q", code, out, e)
	}
	if code, _, _ := runCLI("verify", "-pubkey", pub, "-kind", "index", "-in", msg, "-sig", msg+".sig"); code != 1 {
		t.Fatal("cross-kind verify passed")
	}
	if code, _, _ := runCLI("sign", "-key", key, "-kind", "nope", "-in", msg, "-out", msg+".x"); code != 1 {
		t.Fatal("bad kind accepted")
	}
}

func TestIndexCommandWritesEmptyIndexAndRefusesRegress(t *testing.T) {
	dir := t.TempDir()
	idx := filepath.Join(dir, "index.json")
	if code, out, e := runCLI("index", "-serial", "5", "-lifetime", "21d", "-out", idx); code != 0 {
		t.Fatalf("index: %d %q %q", code, out, e)
	}
	if code, _, e := runCLI("index", "-serial", "5", "-prev", idx, "-out", idx+".2"); code != 1 || !strings.Contains(e, "greater") {
		t.Fatalf("regress: %d %q", code, e)
	}
	if code, _, e := runCLI("index", "-serial", "6", "-prev", idx, "-out", idx+".2"); code != 0 {
		t.Fatalf("resign: %d %q", code, e)
	}
}
