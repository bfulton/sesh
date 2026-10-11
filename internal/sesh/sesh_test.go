package sesh

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bfulton/sesh/internal/store"
	"github.com/bfulton/sesh/internal/token"
	"github.com/bfulton/sesh/internal/vault"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

const transcriptJSONL = `{"type":"user","message":{"role":"user","content":"Remember the codeword garnet4242."}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Noted: garnet4242."},{"type":"tool_use","id":"t1","name":"Edit","input":{"file_path":"app.txt"}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}
`

func TestSaveRestoreEndToEnd(t *testing.T) {
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "remote.git")
	src := filepath.Join(tmp, "src")
	run(t, tmp, "git", "init", "-q", "--bare", "-b", "main", remote)
	run(t, tmp, "git", "clone", "-q", remote, src)
	os.WriteFile(filepath.Join(src, "app.txt"), []byte("v1\n"), 0o644)
	run(t, src, "git", "add", "-A")
	run(t, src, "git", "commit", "-q", "-m", "v1")
	run(t, src, "git", "push", "-q", "origin", "main")
	os.WriteFile(filepath.Join(src, "app.txt"), []byte("v2, uncommitted\n"), 0o644)
	os.WriteFile(filepath.Join(src, "notes.md"), []byte("untracked notes\n"), 0o644)

	tpath := filepath.Join(tmp, "session.jsonl")
	os.WriteFile(tpath, []byte(transcriptJSONL), 0o600)
	os.MkdirAll(filepath.Join(tmp, "memory", "topics"), 0o700)
	os.WriteFile(filepath.Join(tmp, "memory", "MEMORY.md"), []byte("- prefers Go\n"), 0o600)
	os.WriteFile(filepath.Join(tmp, "memory", "topics", "build.md"), []byte("use make\n"), 0o600)
	st := &store.Dir{Root: filepath.Join(tmp, "store")}
	tokDir := filepath.Join(tmp, "tokens")
	ctx := context.Background()

	saved, err := Save(ctx, SaveOptions{Store: st, SessionID: "sess-1", TranscriptPath: tpath, Dir: src, TokenDir: tokDir})
	if err != nil {
		t.Fatal(err)
	}
	if saved.Turns != 2 || saved.Worktree != "" || !saved.Pushed || len(saved.Manifest.Patches) != 2 {
		t.Fatalf("save result: %+v", saved)
	}

	// Saving again keeps the same token.
	again, err := Save(ctx, SaveOptions{Store: st, SessionID: "sess-1", TranscriptPath: tpath, Dir: src, TokenDir: tokDir})
	if err != nil {
		t.Fatal(err)
	}
	if again.Token != saved.Token {
		t.Error("second save of the same session changed its token")
	}

	// Restore from the token's text form into a fresh clone.
	tok, err := token.Parse(strings.ToUpper(saved.Token.String()))
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(tmp, "dst")
	run(t, tmp, "git", "clone", "-q", remote, dst)
	res, err := Restore(ctx, RestoreOptions{Store: st, Token: tok, Dir: dst})
	if err != nil {
		t.Fatal(err)
	}
	if res.Worktree != "" || res.Branch == "" {
		t.Fatalf("working copy not restored: %q", res.Worktree)
	}
	for f, want := range map[string]string{"app.txt": "v2, uncommitted\n", "notes.md": "untracked notes\n"} {
		got, _ := os.ReadFile(filepath.Join(dst, f))
		if string(got) != want {
			t.Errorf("%s = %q, want %q", f, got, want)
		}
	}
	if !strings.Contains(res.Rendered, "garnet4242") || !strings.Contains(res.Rendered, "--- tool call: Edit") {
		t.Errorf("rendered history incomplete:\n%s", res.Rendered)
	}
	if got := run(t, dst, "git", "rev-parse", "--abbrev-ref", "HEAD"); got != res.Branch {
		t.Errorf("on branch %s, want %s", got, res.Branch)
	}

	// Native restore also writes a resumable session for the target directory.
	home := filepath.Join(tmp, "home")
	dst2 := filepath.Join(tmp, "dst2")
	run(t, tmp, "git", "clone", "-q", remote, dst2)
	nres, err := Restore(ctx, RestoreOptions{Store: st, Token: tok, Dir: dst2, Native: true, Home: home})
	if err != nil {
		t.Fatal(err)
	}
	if nres.NativeErr != "" || nres.NativeSession == "" {
		t.Fatalf("native session not written: %q", nres.NativeErr)
	}
	native, err := os.ReadFile(nres.NativePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(native), "garnet4242") || !strings.HasPrefix(nres.NativePath, home) {
		t.Errorf("native session at %s is wrong:\n%s", nres.NativePath, native)
	}
	memDir := filepath.Join(filepath.Dir(nres.NativePath), "memory")
	for f, want := range map[string]string{"MEMORY.md": "- prefers Go\n", "topics/build.md": "use make\n"} {
		got, _ := os.ReadFile(filepath.Join(memDir, f))
		if string(got) != want {
			t.Errorf("memory %s = %q, want %q", f, got, want)
		}
	}

	// A second native restore into the same project keeps edited memory.
	os.WriteFile(filepath.Join(memDir, "MEMORY.md"), []byte("edited locally\n"), 0o600)
	if _, err := Restore(ctx, RestoreOptions{Store: st, Token: tok, Dir: dst2, Native: true, Home: home}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(memDir, "MEMORY.md")); string(got) != "edited locally\n" {
		t.Errorf("restore overwrote existing memory: %q", got)
	}

	// A wrong token finds nothing.
	other, _ := token.New()
	if _, err := Restore(ctx, RestoreOptions{Store: st, Token: other, Dir: dst}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("wrong token: err = %v, want ErrNotFound", err)
	}
}

func TestRestoreHistoryWithoutRepo(t *testing.T) {
	tmp := t.TempDir()
	tpath := filepath.Join(tmp, "session.jsonl")
	os.WriteFile(tpath, []byte(transcriptJSONL), 0o600)
	st := &store.Dir{Root: filepath.Join(tmp, "store")}
	ctx := context.Background()
	saved, err := Save(ctx, SaveOptions{Store: st, SessionID: "s", TranscriptPath: tpath, Dir: tmp, TokenDir: filepath.Join(tmp, "tok")})
	if err != nil {
		t.Fatal(err)
	}
	if saved.Worktree == "" {
		t.Error("expected a note that no working copy was captured")
	}
	res, err := Restore(ctx, RestoreOptions{Store: st, Token: saved.Token, Dir: tmp})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Rendered, "garnet4242") || res.Worktree == "" {
		t.Errorf("history-only restore: worktree=%q", res.Worktree)
	}
}

func TestSessionTokenRejectsPathTricks(t *testing.T) {
	for _, bad := range []string{"", "..", "a/b", `a\b`} {
		if _, err := sessionToken(t.TempDir(), bad); err == nil {
			t.Errorf("sessionToken(%q) succeeded", bad)
		}
	}
}

func TestSaveWithVault(t *testing.T) {
	tmp := t.TempDir()
	tpath := filepath.Join(tmp, "session.jsonl")
	os.WriteFile(tpath, []byte(`{"type":"ai-title","aiTitle":"Codeword drill"}`+"\n"+transcriptJSONL), 0o600)
	st := &store.Dir{Root: filepath.Join(tmp, "store")}
	key, _ := token.New()
	v := &vault.Vault{Key: key}
	ctx := context.Background()

	// Two devices with no shared local state save the same session.
	var toks []token.Token
	for _, dev := range []string{"laptop", "cloud"} {
		res, err := Save(ctx, SaveOptions{Store: st, SessionID: "sess-v", TranscriptPath: tpath, Dir: tmp,
			SkipWorktree: true, Vault: v, TokenDir: filepath.Join(tmp, dev), Host: dev})
		if err != nil {
			t.Fatal(err)
		}
		toks = append(toks, res.Token)
	}
	if toks[0] != toks[1] || toks[0] != v.SessionToken(SessionKey("sess-v", tpath)) {
		t.Fatal("devices sharing a vault saved the session under different tokens")
	}
	entries, bad, err := v.List(ctx, st)
	if err != nil || bad != 0 || len(entries) != 1 {
		t.Fatalf("catalog: %+v bad=%d err=%v", entries, bad, err)
	}
	e := entries[0]
	if e.Title != "Codeword drill" || e.Token != toks[0].String() || e.Host != "cloud" || e.Messages != 2 {
		t.Errorf("entry = %+v", e)
	}
	// The catalog's token opens the session.
	tok, _ := token.Parse(e.Token)
	a, err := Fetch(ctx, st, tok)
	if err != nil || !strings.Contains(string(a.Transcript), "garnet4242") {
		t.Fatalf("Fetch: %v", err)
	}
}

// Two transcripts with the same session id in different project folders are
// different sessions; saving them in either order keeps both.
func TestSameSessionIDInTwoFolders(t *testing.T) {
	tmp := t.TempDir()
	st := &store.Dir{Root: filepath.Join(tmp, "store")}
	key, _ := token.New()
	v := &vault.Vault{Key: key}
	ctx := context.Background()
	mk := func(folder, word string) string {
		p := filepath.Join(tmp, "projects", folder, "same-id.jsonl")
		os.MkdirAll(filepath.Dir(p), 0o700)
		os.WriteFile(p, []byte(`{"type":"user","message":{"role":"user","content":"codeword `+word+`"}}`+"\n"), 0o600)
		return p
	}
	real, nested := mk("-home-user-repo", "real"), mk("-tmp-scratch", "nested")
	for _, order := range [][]string{{real, nested}, {nested, real}} {
		for _, p := range order {
			if _, err := Save(ctx, SaveOptions{Store: st, SessionID: "same-id", TranscriptPath: p, SkipWorktree: true, Vault: v}); err != nil {
				t.Fatal(err)
			}
		}
		entries, _, err := v.List(ctx, st)
		if err != nil || len(entries) != 2 {
			t.Fatalf("entries = %+v, %v; want two sessions", entries, err)
		}
		for _, e := range entries {
			tok, _ := token.Parse(e.Token)
			a, err := Fetch(ctx, st, tok)
			if err != nil {
				t.Fatal(err)
			}
			want := "real"
			if strings.Contains(e.Key, "scratch") {
				want = "nested"
			}
			if !strings.Contains(string(a.Transcript), "codeword "+want) {
				t.Errorf("entry %s holds the other session's transcript", e.Key)
			}
		}
	}
}
