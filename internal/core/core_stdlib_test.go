package core

import "testing"

func TestResolutionConstantsAreDistinct(t *testing.T) {
	if ResRaw == Res1m {
		t.Errorf("ResRaw and Res1m must be distinct, both are %v", ResRaw)
	}
	if Res1m == ResAuto {
		t.Errorf("Res1m and ResAuto must be distinct, both are %v", Res1m)
	}
	if ResRaw == ResAuto {
		t.Errorf("ResRaw and ResAuto must be distinct, both are %v", ResRaw)
	}
}
