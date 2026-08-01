package core

import "testing"

func TestPickResolution(t *testing.T) {
	// Short window: raw resolution is precise enough and cheap to serve.
	if got := PickResolution(0, 3600); got != ResRaw {
		t.Errorf("1h window: got %v want ResRaw", got)
	}
	// Long window: fall back to the coarser 1-minute rollups.
	if got := PickResolution(0, 60*60*24*40); got != Res1m {
		t.Errorf("40d window: got %v want Res1m", got)
	}
}
