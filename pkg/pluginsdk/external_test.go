package pluginsdk_test

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestPluginSourcesAvoidInternalPackages(t *testing.T) {
	var files []string
	for _, pat := range []string{"../../registry/plugins/_template/*.go", "../../plugins/*/*.go"} {
		m, err := filepath.Glob(pat)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	if len(files) == 0 {
		t.Fatal("no plugin sources found")
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") && !strings.Contains(f, "_template") {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, im := range af.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			if strings.Contains(p, "/internal/") || strings.HasSuffix(p, "/internal") {
				t.Errorf("%s imports %s; plugins built outside this module cannot", f, p)
			}
		}
	}
}
