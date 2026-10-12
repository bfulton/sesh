package sesh

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bfulton/sesh/internal/archive"
	"github.com/bfulton/sesh/internal/redact"
	"github.com/bfulton/sesh/internal/store"
)

// fakeToken makes a GitHub-token-shaped string at run time, so none sits in
// the source.
func fakeToken(r *rand.Rand) string {
	const set = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 36)
	for i := range b {
		b[i] = set[r.Intn(len(set))]
	}
	return "ghp_" + string(b)
}

func TestSaveRedactsSecrets(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	inBase, inEdit, inChat, inMemory, inTool := fakeToken(rnd), fakeToken(rnd), fakeToken(rnd), fakeToken(rnd), fakeToken(rnd)

	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote.git")
	src := filepath.Join(tmp, "src")
	run(t, tmp, "git", "init", "-q", "--bare", "-b", "main", remote)
	run(t, tmp, "git", "clone", "-q", remote, src)
	os.WriteFile(filepath.Join(src, ".env"), []byte("NAME=a\nOLD_TOKEN="+inBase+"\n"), 0o644)
	run(t, src, "git", "add", "-A")
	run(t, src, "git", "commit", "-q", "-m", "v1")
	run(t, src, "git", "push", "-q", "origin", "main")
	// The edit keeps the committed secret as context and adds a new one.
	os.WriteFile(filepath.Join(src, ".env"), []byte("NAME=b\nOLD_TOKEN="+inBase+"\nNEW_TOKEN="+inEdit+"\n"), 0o644)

	chat, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": "use GITHUB_TOKEN=" + inChat + " for this"}})
	reply := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Done."}]}}`
	tpath := filepath.Join(tmp, "session.jsonl")
	os.WriteFile(tpath, []byte(string(chat)+"\n"+reply+"\n"), 0o600)
	os.MkdirAll(filepath.Join(tmp, "memory"), 0o700)
	os.WriteFile(filepath.Join(tmp, "memory", "MEMORY.md"), []byte("- token: "+inMemory+"\n"), 0o600)
	os.MkdirAll(filepath.Join(tmp, "session", "tool-results"), 0o700)
	os.WriteFile(filepath.Join(tmp, "session", "tool-results", "out.txt"), []byte("env:\nGH_TOKEN="+inTool+"\n"), 0o600)

	st := &store.Dir{Root: filepath.Join(tmp, "store")}
	ctx := context.Background()
	cache := filepath.Join(tmp, "scanned", "sess-r")
	saved, err := Save(ctx, SaveOptions{Store: st, SessionID: "sess-r", TranscriptPath: tpath, Dir: src,
		TokenDir: filepath.Join(tmp, "tokens"), ScanCache: cache})
	if err != nil {
		t.Fatal(err)
	}
	if saved.Redacted != 4 {
		t.Errorf("Redacted = %d, want 4: %v", saved.Redacted, saved.Manifest.Redacted)
	}
	want := map[string]int{
		archive.TranscriptName:                   1,
		archive.MemoryName("MEMORY.md"):          1,
		archive.FileName("tool-results/out.txt"): 1,
		archive.PatchName(".env"):                1,
	}
	for name, n := range want {
		if saved.Manifest.Redacted[name] != n {
			t.Errorf("Redacted[%s] = %d, want %d", name, saved.Manifest.Redacted[name], n)
		}
	}
	if _, err := os.Stat(cache); err != nil {
		t.Errorf("no scan cache: %v", err)
	}

	// The stored archive holds none of the new secrets.
	a, err := Fetch(ctx, st, saved.Token)
	if err != nil {
		t.Fatal(err)
	}
	all := string(a.Transcript) + string(a.Memory["MEMORY.md"]) + string(a.Files["tool-results/out.txt"]) + string(a.Patches[".env"])
	for _, s := range []string{inEdit, inChat, inMemory, inTool} {
		if strings.Contains(all, s) {
			t.Error("a secret reached the archive")
		}
	}
	if !strings.Contains(string(a.Transcript), "use GITHUB_TOKEN=[REDACTED:github-pat] for this") {
		t.Errorf("transcript:\n%s", a.Transcript)
	}
	if a.Manifest.Redacted[archive.TranscriptName] != 1 {
		t.Error("the stored manifest doesn't record the redaction")
	}

	// The patch still applies: its context line, from the base commit, is
	// as it was.
	dst := filepath.Join(tmp, "dst")
	run(t, tmp, "git", "clone", "-q", remote, dst)
	res, err := Restore(ctx, RestoreOptions{Store: st, Token: saved.Token, Dir: dst})
	if err != nil {
		t.Fatal(err)
	}
	if res.Worktree != "" {
		t.Fatalf("working copy not restored: %s", res.Worktree)
	}
	got, _ := os.ReadFile(filepath.Join(dst, ".env"))
	if string(got) != "NAME=b\nOLD_TOKEN="+inBase+"\nNEW_TOKEN=[REDACTED:github-pat]\n" {
		t.Errorf(".env restored as:\n%s", got)
	}

	// KeepSecrets saves them as they are.
	kept, err := Save(ctx, SaveOptions{Store: st, SessionID: "sess-r", TranscriptPath: tpath, Dir: src,
		TokenDir: filepath.Join(tmp, "tokens"), KeepSecrets: true})
	if err != nil {
		t.Fatal(err)
	}
	a, _ = Fetch(ctx, st, kept.Token)
	if kept.Redacted != 0 || !strings.Contains(string(a.Transcript), inChat) {
		t.Error("KeepSecrets still redacted")
	}

	// A save out of time uploads nothing.
	os.WriteFile(tpath, []byte(string(chat)+"\n"+reply+"\n"+`{"type":"user","message":{"role":"user","content":"more"}}`+"\n"), 0o600)
	_, err = Save(ctx, SaveOptions{Store: st, SessionID: "sess-r", TranscriptPath: tpath, Dir: src,
		TokenDir: filepath.Join(tmp, "tokens"), ScanCache: cache, ScanBudget: time.Nanosecond})
	var inc *redact.Incomplete
	if !errors.As(err, &inc) {
		t.Fatalf("Save out of time = %v, want *redact.Incomplete", err)
	}
	a, _ = Fetch(ctx, st, kept.Token)
	if !strings.Contains(string(a.Transcript), inChat) {
		t.Error("a save out of time replaced the stored archive")
	}
}
