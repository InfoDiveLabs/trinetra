package registry

import (
	"reflect"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
	"github.com/InfoDiveLabs/trinetra/internal/update"
)

func TestRegistryTrustsTheReleaseKeys(t *testing.T) {
	if !reflect.DeepEqual(Keys(), update.ProductionKeys()) {
		t.Fatal("registry keys differ from the release key set")
	}
}

func TestPluginPrefixesDifferFromReleasePrefixes(t *testing.T) {
	for _, p := range []string{plugin.ManifestSigPrefix, plugin.IndexSigPrefix} {
		if p == update.ReleasePrefix || p == update.ChannelPrefix {
			t.Fatalf("plugin prefix %q collides with a release prefix", p)
		}
	}
}
