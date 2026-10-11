package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bfulton/sesh/internal/sesh"
	"github.com/bfulton/sesh/internal/store"
	"github.com/bfulton/sesh/internal/token"
	"github.com/bfulton/sesh/internal/vault"
)

// runCmd runs a sesh subcommand and returns what it printed.
func runCmd(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	capture := func(f **os.File) (done func() string) {
		r, w, perr := os.Pipe()
		if perr != nil {
			t.Fatal(perr)
		}
		old := *f
		*f = w
		out := make(chan string)
		go func() {
			b, _ := io.ReadAll(r)
			out <- string(b)
		}()
		return func() string {
			w.Close()
			*f = old
			return <-out
		}
	}
	outDone, errDone := capture(&os.Stdout), capture(&os.Stderr)
	err = subcommands[args[0]](args[1:])
	return outDone(), errDone(), err
}

// claudeEnv sets the variables that tell sesh where it runs: inside Claude
// Code or not, and in a cloud session or not.
func claudeEnv(t *testing.T, inClaude, cloud bool) {
	t.Setenv("CLAUDECODE", map[bool]string{true: "1"}[inClaude])
	t.Setenv("CLAUDE_CODE_REMOTE", map[bool]string{true: "true"}[cloud])
}

// The vault key is never printed inside a Claude Code session, whose
// transcript would keep it, and no vault is set up in a cloud session,
// whose files are lost when it ends.
func TestKeyStaysOutOfSessions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SESH_HOME", home)
	t.Setenv("SESH_KEY", "")
	t.Setenv("SESH_STORE", "")

	claudeEnv(t, true, true)
	if _, _, err := runCmd(t, "init"); err == nil || !strings.Contains(err.Error(), "SESH_KEY and SESH_STORE") {
		t.Errorf("init in a cloud session: %v; want a refusal", err)
	}
	if _, err := os.Stat(filepath.Join(home, "key")); err == nil {
		t.Error("init in a cloud session wrote a key")
	}

	claudeEnv(t, true, false)
	out, _, err := runCmd(t, "init")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, "key"))
	if err != nil {
		t.Fatal(err)
	}
	key := strings.TrimSpace(string(data))
	if strings.Contains(out, key) || !strings.Contains(out, "sesh key in your own terminal") {
		t.Errorf("init inside Claude Code printed the key or no pointer:\n%s", out)
	}
	if out, _, err := runCmd(t, "key"); err == nil || strings.Contains(out, key) {
		t.Errorf("key inside Claude Code: %v, printed %q", err, out)
	}

	claudeEnv(t, false, false)
	if out, _, err := runCmd(t, "key"); err != nil || strings.TrimSpace(out) != key {
		t.Errorf("key in a terminal: %v, %q", err, out)
	}
}

// Locally a restore writes a session that claude --resume continues. In a
// cloud session, where that file would never be read, it prints the history
// for the model, and says where the rest can be read.
func TestRestoreLocalAndCloud(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SESH_HOME", filepath.Join(home, ".sesh"))
	key, _ := token.New()
	t.Setenv("SESH_KEY", key.String())
	t.Setenv("SESH_STORE", "file://"+filepath.Join(home, "store"))
	tp := filepath.Join(home, "s.jsonl")
	os.WriteFile(tp, []byte(`{"type":"user","message":{"role":"user","content":"Which alphabet for share tokens?"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Crockford base32."}]}}
`), 0o600)
	if _, err := sesh.Save(ctx, sesh.SaveOptions{
		Store: &store.Dir{Root: filepath.Join(home, "store")}, SessionID: "abcdef12-0000", TranscriptPath: tp,
		SkipWorktree: true, Vault: &vault.Vault{Key: key},
	}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	projects := filepath.Join(home, ".claude", "projects")

	claudeEnv(t, true, true)
	out, _, err := runCmd(t, "restore", "-dir", dir, "abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "[sesh] Restored record") || !strings.Contains(out, "Crockford base32.") ||
		!strings.Contains(out, "search_messages and read_messages") || strings.Contains(out, "claude --resume") {
		t.Errorf("cloud restore printed:\n%s", out)
	}
	if _, err := os.Stat(projects); err == nil {
		t.Error("cloud restore wrote a native session")
	}

	claudeEnv(t, false, false)
	out, _, err = runCmd(t, "restore", "-dir", dir, "abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "claude --resume ") || strings.Contains(out, "Crockford") {
		t.Errorf("local restore printed:\n%s", out)
	}
	if m, _ := filepath.Glob(filepath.Join(projects, "*", "*.jsonl")); len(m) != 1 {
		t.Errorf("local restore wrote %d native sessions, want 1", len(m))
	}
}

func TestStatus(t *testing.T) {
	t.Setenv("SESH_HOME", t.TempDir())
	t.Setenv("SESH_KEY", "")
	t.Setenv("SESH_STORE", "https://user:secret@sesh.example")
	out, _, err := runCmd(t, "status")
	if err != nil || !strings.Contains(out, "vault: none") || !strings.Contains(out, "store: https://…@sesh.example") || strings.Contains(out, "secret") {
		t.Errorf("status: %v\n%s", err, out)
	}
}
