package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// When one session id has transcripts in two project folders, sync takes
// the most recently modified: the session's current home.
func TestLocalTranscriptsOnePerSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	put := func(folder, id string, age time.Duration) string {
		p := filepath.Join(home, ".claude", "projects", folder, id+".jsonl")
		os.MkdirAll(filepath.Dir(p), 0o700)
		os.WriteFile(p, []byte("{}\n"), 0o600)
		ts := time.Now().Add(-age)
		os.Chtimes(p, ts, ts)
		return p
	}
	put("-repo", "moved", time.Hour)
	moved := put("-repo-worktree", "moved", time.Minute)
	other := put("-repo", "other", time.Hour)
	got, err := localTranscripts()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{moved, other} // sorted: "-repo-worktree/" < "-repo/"
	if !reflect.DeepEqual(got, want) {
		t.Errorf("localTranscripts = %v, want %v", got, want)
	}
}
