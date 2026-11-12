package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readDocs(t *testing.T) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "docs", "plugins", "*.md"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no plugin docs found: %v", err)
	}
	var b strings.Builder
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(data)
	}
	return b.String()
}

func TestDocsCoverEveryPermissionSlotAndWidget(t *testing.T) {
	docs := readDocs(t)
	var terms []string
	for _, p := range V1Permissions {
		terms = append(terms, string(p), Describe(p))
	}
	for _, p := range ReservedPermissions {
		terms = append(terms, string(p))
	}
	for _, s := range V1Slots {
		terms = append(terms, string(s))
	}
	for _, k := range []WidgetKind{WidgetTimeseries, WidgetStat, WidgetTable, WidgetKV, WidgetLog, WidgetForm, WidgetAction, WidgetText, WidgetStatus} {
		terms = append(terms, string(k))
	}
	for _, term := range terms {
		if !strings.Contains(docs, term) {
			t.Errorf("docs/plugins does not mention %q", term)
		}
	}
}

func TestDocsCoverEveryManifestField(t *testing.T) {
	docs := readDocs(t)
	fields := []string{"schema", "id", "name", "version", "publisher", "description", "homepage", "host_api", "min_trinetra",
		"permissions", "binaries", "config_schema", "contributes", "activation", "slots", "actions", "files",
		"min_role", "confirm", "mutating", "content_type", "x-order", "x-section", "format", "pattern", "default", "maxItems"}
	for _, f := range fields {
		if !strings.Contains(docs, "`"+f+"`") {
			t.Errorf("docs/plugins does not document manifest field `%s`", f)
		}
	}
}
