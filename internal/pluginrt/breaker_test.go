package pluginrt

import (
	"testing"
	"time"
)

func TestBreakerTripsAtFifthCrashWithinWindow(t *testing.T) {
	now := time.Unix(0, 0)
	b := NewBreaker(func() time.Time { return now })
	for i := 1; i <= 4; i++ {
		if b.Crash() {
			t.Fatalf("tripped at %d", i)
		}
		now = now.Add(time.Minute)
	}
	if !b.Crash() || !b.Tripped() {
		t.Fatal("expected trip at 5th")
	}
}

func TestBreakerIgnoresCrashesOutsideWindow(t *testing.T) {
	now := time.Unix(0, 0)
	b := NewBreaker(func() time.Time { return now })
	for i := 0; i < 20; i++ {
		if b.Crash() {
			t.Fatalf("tripped at %d", i)
		}
		now = now.Add(3 * time.Minute)
	}
}

func TestBreakerTripsOnce(t *testing.T) {
	now := time.Unix(0, 0)
	b := NewBreaker(func() time.Time { return now })
	trips := 0
	for i := 0; i < 8; i++ {
		if b.Crash() {
			trips++
		}
	}
	if trips != 1 {
		t.Fatalf("trips = %d", trips)
	}
}

func TestBreakerResetClears(t *testing.T) {
	now := time.Unix(0, 0)
	b := NewBreaker(func() time.Time { return now })
	for i := 0; i < 5; i++ {
		b.Crash()
	}
	b.Reset()
	if b.Tripped() {
		t.Fatal("still tripped")
	}
	for i := 0; i < 4; i++ {
		if b.Crash() {
			t.Fatal("tripped too early after reset")
		}
	}
	if !b.Crash() {
		t.Fatal("expected trip after reset")
	}
}
