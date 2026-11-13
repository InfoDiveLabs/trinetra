package registry

import "github.com/InfoDiveLabs/trinetra/internal/update"

// Keys returns the keys plugin signatures are checked against: the release key set compiled
// into this build. Manifests need a CI and a maintainer signature; the index is signed with the pointer key.
func Keys() update.KeySet { return update.ProductionKeys() }
