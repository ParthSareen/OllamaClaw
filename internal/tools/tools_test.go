package tools

import "testing"

func TestTruncate(t *testing.T) {
	in := "abcdefghijklmnopqrstuvwxyz"
	out := truncate(in, 10)
	if len(out) > 10 {
		t.Fatalf("expected truncated output <= 10, got %d", len(out))
	}
}

func TestAsInt(t *testing.T) {
	if v, ok := asInt(float64(3)); !ok || v != 3 {
		t.Fatalf("asInt(float64) failed")
	}
	if _, ok := asInt("3"); ok {
		t.Fatalf("asInt should fail for string")
	}
}
