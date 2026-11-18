package main

import (
	"testing"
	"time"
)

func TestJitter(t *testing.T) {
	for _, d := range []time.Duration{0, 1, time.Minute} {
		for range 1000 {
			if j := jitter(d); j < d*3/4 || j > d*5/4 {
				t.Fatalf("jitter(%v) = %v, want within a quarter of it", d, j)
			}
		}
	}
}
