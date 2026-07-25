package serverwatch

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestDumpSeriesCSV(t *testing.T) {
	store, err := OpenStore("memory", t.TempDir(), StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_ = store.Append(100, MetricSet{"cpu": 10})
	_ = store.Append(200, MetricSet{"cpu": 20})

	var buf bytes.Buffer
	if err := dumpSeries(&buf, store, "cpu", 0, 1000, ResRaw, "csv"); err != nil {
		t.Fatal(err)
	}
	want := "ts,min,avg,max\n100,10,10,10\n200,20,20,20\n"
	if got := buf.String(); got != want {
		t.Fatalf("csv = %q, want %q", got, want)
	}
}

func TestDumpSeriesJSON(t *testing.T) {
	store, err := OpenStore("memory", t.TempDir(), StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_ = store.Append(100, MetricSet{"cpu": 10})

	var buf bytes.Buffer
	if err := dumpSeries(&buf, store, "cpu", 0, 1000, ResRaw, "json"); err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(buf.String())
	want := `[{"TS":100,"Min":10,"Avg":10,"Max":10}]`
	if got != want {
		t.Fatalf("json = %q, want %q", got, want)
	}
}

func TestDumpSeriesEmptyMetric(t *testing.T) {
	store, err := OpenStore("memory", t.TempDir(), StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	var csvBuf bytes.Buffer
	if err := dumpSeries(&csvBuf, store, "nosuchmetric", 0, 1000, ResRaw, "csv"); err != nil {
		t.Fatal(err)
	}
	if got := csvBuf.String(); got != "ts,min,avg,max\n" {
		t.Fatalf("empty csv = %q, want header only", got)
	}

	var jsonBuf bytes.Buffer
	if err := dumpSeries(&jsonBuf, store, "nosuchmetric", 0, 1000, ResRaw, "json"); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(jsonBuf.String()); got != "[]" {
		t.Fatalf("empty json = %q, want []", got)
	}
}

func TestDumpSeriesUnknownFormat(t *testing.T) {
	store, err := OpenStore("memory", t.TempDir(), StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var buf bytes.Buffer
	if err := dumpSeries(&buf, store, "cpu", 0, 1000, ResRaw, "xml"); err == nil {
		t.Fatal("expected error for unknown format")
	}
}

func TestDumpCLISmokeEmptyStateDir(t *testing.T) {
	dir := t.TempDir()
	cfgPath = filepath.Join(dir, "config.json")
	stateDir = filepath.Join(dir, "state")

	var out bytes.Buffer
	stdout = &out
	if code := Main([]string{"dump", "--metric", "cpu"}); code != 0 {
		t.Fatalf("dump on empty state dir exit=%d, stdout=%q", code, out.String())
	}
	if got := out.String(); got != "ts,min,avg,max\n" {
		t.Fatalf("stdout = %q, want header-only csv", got)
	}
}

func TestDumpCLIRequiresMetric(t *testing.T) {
	dir := t.TempDir()
	cfgPath = filepath.Join(dir, "config.json")
	stateDir = filepath.Join(dir, "state")

	var out bytes.Buffer
	stdout = &out
	if code := Main([]string{"dump"}); code == 0 {
		t.Fatal("dump without --metric should be a usage error")
	}
}
