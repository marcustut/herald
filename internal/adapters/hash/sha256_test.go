package hash

import "testing"

func TestSHA256(t *testing.T) {
	h := SHA256{}
	const empty = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got := h.Hash(""); got != empty {
		t.Fatalf("Hash(\"\") = %q", got)
	}
	if h.Hash("a") == h.Hash("b") {
		t.Fatal("distinct inputs must not collide")
	}
	first := h.Hash("a")
	if h.Hash("a") != first {
		t.Fatal("hash must be deterministic")
	}
}
