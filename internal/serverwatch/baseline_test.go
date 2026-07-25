package serverwatch

import "testing"

func TestBaselineZScore(t *testing.T) {
	b := NewBaseline()
	for i := 0; i < 200; i++ {
		b.Observe("cpu", 10) // stable around 10
	}
	if _, ready := b.Z("cpu", 10); !ready {
		t.Fatal("should be ready after 200 obs")
	}
	z, _ := b.Z("cpu", 60) // big spike
	if z < 3 {
		t.Fatalf("spike z = %v, expected >= 3", z)
	}
	zn, _ := b.Z("cpu", 10)
	if zn > 1 {
		t.Fatalf("norm z = %v, expected small", zn)
	}
}

func TestBaselineNotReadyEarly(t *testing.T) {
	b := NewBaseline()
	b.Observe("mem", 50)
	if _, ready := b.Z("mem", 90); ready {
		t.Fatal("should not be ready with 1 obs")
	}
}
