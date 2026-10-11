package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSessionIDsDisambiguates(t *testing.T) {
	dir := t.TempDir()
	mk := func(proj, id string, age time.Duration) string {
		p := filepath.Join(dir, proj, id+".jsonl")
		os.MkdirAll(filepath.Dir(p), 0o700)
		os.WriteFile(p, []byte("{}\n"), 0o600)
		ts := time.Now().Add(-age)
		os.Chtimes(p, ts, ts)
		return p
	}
	live := mk("-home-user-repo", "abc", 0)
	nested := mk("-tmp-scratch", "abc", time.Hour)
	other := mk("-home-user-repo", "def", 0)
	ids := sessionIDs([]string{nested, live, other})
	if ids[live] != "abc" || ids[other] != "def" {
		t.Errorf("ids = %v", ids)
	}
	if ids[nested] == "abc" || len(ids[nested]) != len("abc-")+6 {
		t.Errorf("duplicate id not disambiguated: %q", ids[nested])
	}
	if again := sessionIDs([]string{live, nested, other}); again[nested] != ids[nested] {
		t.Error("disambiguated id is not stable")
	}
}
