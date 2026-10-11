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
