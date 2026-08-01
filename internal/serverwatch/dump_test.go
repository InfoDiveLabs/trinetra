package serverwatch

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"serverwatch/internal/config"
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

// TestDumpCLIRoutesThroughFileAPIPreservesFormat is a golden-style test for
// the task-6 routing: cmdDump now calls newFileAPI(...).Series(...) instead
// of opening a SampleStore and calling dumpSeries directly, and this pins
// that the CSV and JSON output stay byte-identical to before -- in
// particular, the JSON keys must stay capitalized ("TS"/"Min"/"Avg"/"Max",
// Point's bare field names) rather than the lowercase "ts"/"min"/"avg"/"max"
// core.SeriesPoint's own json tags would produce if marshaled directly.
func TestDumpCLIRoutesThroughFileAPIPreservesFormat(t *testing.T) {
	dir := t.TempDir()
	cfgPath = filepath.Join(dir, "config.json")
	stateDir = filepath.Join(dir, "state")

	store, err := openConfiguredStore(config.Default())
	if err != nil {
		t.Fatalf("openConfiguredStore: %v", err)
	}
	if err := store.Append(100, MetricSet{"cpu": 10}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := store.Append(200, MetricSet{"cpu": 20}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var out bytes.Buffer
	stdout = &out
	// A --since window wide enough to cover the epoch-relative timestamps
	// (100/200) appended above relative to "now".
	if code := Main([]string{"dump", "--metric", "cpu", "--since", "999999h", "--format", "csv"}); code != 0 {
		t.Fatalf("dump csv exit=%d, stdout=%q", code, out.String())
	}
	wantCSV := "ts,min,avg,max\n100,10,10,10\n200,20,20,20\n"
	if got := out.String(); got != wantCSV {
		t.Fatalf("csv = %q, want %q", got, wantCSV)
	}

	out.Reset()
	if code := Main([]string{"dump", "--metric", "cpu", "--since", "999999h", "--format", "json"}); code != 0 {
		t.Fatalf("dump json exit=%d, stdout=%q", code, out.String())
	}
	wantJSON := `[{"TS":100,"Min":10,"Avg":10,"Max":10},{"TS":200,"Min":20,"Avg":20,"Max":20}]`
	if got := strings.TrimSpace(out.String()); got != wantJSON {
		t.Fatalf("json = %q, want %q", got, wantJSON)
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
