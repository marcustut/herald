package secret

import (
	"encoding/hex"
	"testing"
)

func TestGenerator(t *testing.T) {
	g := Generator{}
	a := g.NewSecret()
	b := g.NewSecret()
	if len(a) != secretBytes*2 {
		t.Fatalf("length = %d", len(a))
	}
	if _, err := hex.DecodeString(a); err != nil {
		t.Fatalf("not hex: %v", err)
	}
	if a == b {
		t.Fatal("secrets must be unique")
	}
}
