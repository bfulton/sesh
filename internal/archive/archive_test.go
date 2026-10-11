package archive

import (
	"bytes"
	"crypto/rand"
	"reflect"
	"strings"
	"testing"
	"time"
)

func key(t *testing.T) []byte {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func sample() *Archive {
	return &Archive{
		Manifest: Manifest{
			Source:  "claude-code",
			Session: "6ffde582-0000-4000-8000-000000000000",
			SavedAt: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC),
			Repo:    "https://github.com/bfulton/sesh",
			Branch:  "main",
			Base:    "37af927",
			Deleted: []string{"old/gone.txt"},
		},
		Transcript: []byte(`{"type":"user"}` + "\n"),
		Patches: map[string][]byte{
			"src/app.go": []byte("diff --git a/src/app.go b/src/app.go\n"),
			"big.bin":    bytes.Repeat([]byte{0, 1, 2, 255}, 1000),
		},
		Memory: map[string][]byte{
			"MEMORY.md":       []byte("- prefers Go\n"),
			"topics/build.md": []byte("use make\n"),
		},
	}
}

func TestRoundTrip(t *testing.T) {
	k := key(t)
	in := sample()
	blob, err := Seal(in, k)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Open(blob, k)
	if err != nil {
		t.Fatal(err)
	}
	if out.Manifest.Sesh != Version {
		t.Errorf("version = %d", out.Manifest.Sesh)
	}
	if want := []string{"big.bin", "src/app.go"}; !reflect.DeepEqual(out.Manifest.Patches, want) {
		t.Errorf("patches = %v, want %v", out.Manifest.Patches, want)
	}
	if !reflect.DeepEqual(out.Memory, in.Memory) {
		t.Error("memory differs after round trip")
	}
	if !reflect.DeepEqual(out.Patches, in.Patches) || !bytes.Equal(out.Transcript, in.Transcript) {
		t.Error("contents differ after round trip")
	}
	if !reflect.DeepEqual(out.Manifest.Deleted, in.Manifest.Deleted) || !out.Manifest.SavedAt.Equal(in.Manifest.SavedAt) {
		t.Error("manifest differs after round trip")
	}
}

func TestWrongKeyAndTamper(t *testing.T) {
	k := key(t)
	blob, err := Seal(sample(), k)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(blob, key(t)); err == nil || !strings.Contains(err.Error(), "wrong token") {
		t.Errorf("wrong key: err = %v", err)
	}
	blob[len(blob)/2] ^= 1
	if _, err := Open(blob, k); err == nil {
		t.Error("tampered blob opened")
	}
	if _, err := Open(blob[:5], k); err == nil {
		t.Error("truncated blob opened")
	}
}

func TestSealIsRandomized(t *testing.T) {
	k := key(t)
	a, _ := Seal(sample(), k)
	b, _ := Seal(sample(), k)
	if bytes.Equal(a, b) {
		t.Error("two seals of the same archive are identical; nonce is not random")
	}
}
