package serverwatch

import (
	"math"
	"testing"
)

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

func TestBaselineZScoreVaryingData(t *testing.T) {
	b := NewBaseline()
	vals := []float64{8, 9, 10, 11, 12}
	for i := 0; i < 400; i++ {
		b.Observe("cpu", vals[i%len(vals)])
	}
	z, ready := b.Z("cpu", 10)
	if !ready {
		t.Fatal("should be ready after 400 obs")
	}
	if math.Abs(z) >= 2 {
		t.Fatalf("in-range z = %v, expected |z| < 2", z)
	}
	zo, _ := b.Z("cpu", 100)
	if zo < 3 {
		t.Fatalf("outlier z = %v, expected >= 3", zo)
	}
}

func TestBaselineNotReadyEarly(t *testing.T) {
	b := NewBaseline()
	b.Observe("mem", 50)
	if _, ready := b.Z("mem", 90); ready {
		t.Fatal("should not be ready with 1 obs")
	}
}
