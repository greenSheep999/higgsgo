package apikey

import (
	"strings"
	"testing"
)

func TestGenerateParseRoundtrip(t *testing.T) {
	pt, hash, err := Generate(KindProject)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !strings.HasPrefix(pt, Prefix) {
		t.Errorf("plaintext missing prefix: %q", pt)
	}
	if len(pt) != len(Prefix)+40 {
		t.Errorf("plaintext wrong length: %d", len(pt))
	}
	h2, err := Parse(pt)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if h2 != hash {
		t.Errorf("hash mismatch: %s vs %s", h2, hash)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, in := range []string{
		"", "sk-hg-",
		"sk-hg-tooshort",
		"nope-hg-0123456789abcdef0123456789abcdef01234567",
		"sk-hg-ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ",
	} {
		if _, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) should have failed", in)
		}
	}
}

func TestHashDeterministic(t *testing.T) {
	pt := "sk-hg-0123456789abcdef0123456789abcdef01234567"
	if Hash(pt) != Hash(pt) {
		t.Fatal("hash not deterministic")
	}
	if Hash(pt) == Hash(pt+"x") {
		t.Fatal("hash collision on trivial change")
	}
}
