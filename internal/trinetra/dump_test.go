package trinetra

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

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

// TestDumpCLIRoutesThroughFileAPIPreservesFormat pins that cmdDump's CSV and
// JSON output stays byte-identical when routed through newFileAPI(...).Series:
// in particular the JSON keys stay capitalized ("TS"/"Min"/"Avg"/"Max", Point's
// field names), not the lowercase tags core.SeriesPoint would marshal to.
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

// TestDumpCLIStoreOpenFailurePreservesErrorText pins, end to end, that
// cmdDump's stderr on a store-open failure is "open sample store: <err>"
// (Series' prefix is covered by TestFileAPISeriesStoreOpenFailureWrapsErrorText).
//
// The config is written directly, bypassing config.Set's
// validateStorageBackend, because config.Load does no such validation; that is
// how an invalid storage.backend reaches openConfiguredStore via loadCfg().
func TestDumpCLIStoreOpenFailurePreservesErrorText(t *testing.T) {
	dir := t.TempDir()
	cfgPath = filepath.Join(dir, "config.json")
	stateDir = filepath.Join(dir, "state")
	if err := os.WriteFile(cfgPath, []byte(`{"storage":{"backend":"bogus"}}`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var out, errb bytes.Buffer
	stdout = &out
	stderr = &errb
	if code := Main([]string{"dump", "--metric", "cpu"}); code == 0 {
		t.Fatalf("expected non-zero exit for an unopenable store, got stdout=%q", out.String())
	}
	if got := errb.String(); !strings.HasPrefix(got, "open sample store: ") {
		t.Fatalf("stderr = %q, want it to start with %q", got, "open sample store: ")
	}
}
