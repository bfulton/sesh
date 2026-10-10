package token

import (
	"bytes"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	for i := 0; i < 1000; i++ {
		tok, err := New()
		if err != nil {
			t.Fatal(err)
		}
		s := tok.String()
		if len(s) != Len {
			t.Fatalf("len(%q) = %d, want %d", s, len(s), Len)
		}
		for _, c := range s {
			if !strings.ContainsRune(alphabet, c) {
				t.Fatalf("%q contains %q, outside the alphabet", s, c)
			}
		}
		got, err := Parse(s)
		if err != nil {
			t.Fatalf("Parse(%q): %v", s, err)
		}
		if got != tok {
			t.Fatalf("Parse(%q) = %x, want %x", s, got, tok)
		}
	}
}

func TestKnownValues(t *testing.T) {
	var zero, ones Token
	for i := range ones {
		ones[i] = 0xff
	}
	if got, want := zero.String(), strings.Repeat("0", Len); got != want {
		t.Errorf("zero = %q, want %q", got, want)
	}
	// 128 one-bits: 25 full groups of 31 ('z'), then 3 one-bits padded with two zeros: 0b11100 = 28 ('w').
	if got, want := ones.String(), strings.Repeat("z", 25)+"w"; got != want {
		t.Errorf("ones = %q, want %q", got, want)
	}
}

func TestParseLenient(t *testing.T) {
	tok, _ := New()
	s := tok.String()
	variants := []string{
		strings.ToUpper(s),
		"  " + s + "\n",
		strings.NewReplacer("1", "l", "0", "o").Replace(s),
		strings.NewReplacer("1", "I", "0", "O").Replace(s),
	}
	for _, v := range variants {
		got, err := Parse(v)
		if err != nil || got != tok {
			t.Errorf("Parse(%q) = %x, %v; want %x", v, got, err, tok)
		}
	}
}

func TestParseRejects(t *testing.T) {
	tok, _ := New()
	s := tok.String()
	bad := []string{
		"",
		s[:Len-1],
		s + "0",
		s[:5] + "u" + s[6:],
		s[:5] + "-" + s[6:],
		s[:Len-1] + "1", // nonzero padding bits
	}
	for _, b := range bad {
		if _, err := Parse(b); err == nil {
			t.Errorf("Parse(%q) succeeded, want error", b)
		}
	}
}

func TestDerivations(t *testing.T) {
	a, _ := New()
	b, _ := New()
	if len(a.Key()) != 32 {
		t.Fatalf("key length %d", len(a.Key()))
	}
	if len(a.ID()) != 32 {
		t.Fatalf("id length %d", len(a.ID()))
	}
	if !bytes.Equal(a.Key(), a.Key()) || a.ID() != a.ID() {
		t.Fatal("derivations are not deterministic")
	}
	if bytes.Equal(a.Key(), b.Key()) || a.ID() == b.ID() {
		t.Fatal("different tokens derived the same key or id")
	}
}
