package trinetra

import (
	"testing"
	"time"
)

func TestShutdownHooksRunInReverseOrder(t *testing.T) {
	var h shutdownHooks
	var got []int
	h.add(func() { got = append(got, 1) })
	h.add(func() { got = append(got, 2) })
	h.run(time.Second)
	if len(got) != 2 || got[0] != 2 || got[1] != 1 {
		t.Fatalf("order %v", got)
	}
}

func TestShutdownHooksGiveUpAfterTimeout(t *testing.T) {
	var h shutdownHooks
	block := make(chan struct{})
	defer close(block)
	h.add(func() { <-block })
	start := time.Now()
	h.run(50 * time.Millisecond)
	if time.Since(start) > time.Second {
		t.Fatal("run waited past its timeout")
	}
}
