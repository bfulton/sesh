// Package token implements sesh tokens: 128 random bits written as 26
// characters of lowercase Crockford base32. A token both locates a saved
// session and decrypts it.
package token

import (
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Size is the number of random bytes in a token.
const Size = 16

// Len is the length of a token's text form.
const Len = 26

// alphabet is Crockford base32 in lowercase: digits and letters without i, l, o, u.
const alphabet = "0123456789abcdefghjkmnpqrstvwxyz"

// Token is a sesh token.
type Token [Size]byte

// New returns a random token.
func New() (Token, error) {
	var t Token
	if _, err := rand.Read(t[:]); err != nil {
		return Token{}, err
	}
	return t, nil
}

// String returns the token's 26-character text form.
func (t Token) String() string {
	// 128 bits in 5-bit groups is 26 groups with 2 bits of padding at the end.
	var b strings.Builder
	b.Grow(Len)
	var acc uint32
	bits := 0
	for _, c := range t {
		acc = acc<<8 | uint32(c)
		bits += 8
		for bits >= 5 {
			bits -= 5
			b.WriteByte(alphabet[(acc>>bits)&31])
		}
	}
	if bits > 0 {
		b.WriteByte(alphabet[(acc<<(5-bits))&31])
	}
	return b.String()
}

// Parse decodes a token's text form. It ignores case and surrounding space,
// and reads i and l as 1 and o as 0.
func Parse(s string) (Token, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) != Len {
		return Token{}, fmt.Errorf("token must be %d characters, got %d", Len, len(s))
	}
	var t Token
	var acc uint32
	bits, n := 0, 0
	for i := 0; i < len(s); i++ {
		v, err := value(s[i])
		if err != nil {
			return Token{}, err
		}
		acc = acc<<5 | uint32(v)
		bits += 5
		if bits >= 8 {
			bits -= 8
			t[n] = byte(acc >> bits)
			n++
		}
	}
	// The final 2 bits are padding and must be zero, so each token has one spelling.
	if acc&(1<<bits-1) != 0 {
		return Token{}, errors.New("token is not valid: bad final character")
	}
	return t, nil
}

func value(c byte) (byte, error) {
	switch c {
	case 'i', 'l':
		c = '1'
	case 'o':
		c = '0'
	}
	if i := strings.IndexByte(alphabet, c); i >= 0 {
		return byte(i), nil
	}
	return 0, fmt.Errorf("token is not valid: unexpected character %q", c)
}

// Key returns the 32-byte encryption key derived from the token.
func (t Token) Key() []byte {
	return derive(t, "sesh key v1", 32)
}

// ID returns the object id derived from the token, as 32 hex characters.
// It reveals nothing about the key.
func (t Token) ID() string {
	return hex.EncodeToString(derive(t, "sesh id v1", 16))
}

func derive(t Token, info string, n int) []byte {
	out, err := hkdf.Key(sha256.New, t[:], nil, info, n)
	if err != nil {
		panic(err) // only fails for lengths far beyond what sesh asks for
	}
	return out
}
