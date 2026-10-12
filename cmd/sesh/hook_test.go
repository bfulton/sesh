package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bfulton/sesh/internal/sesh"
	"github.com/bfulton/sesh/internal/store"
	"github.com/bfulton/sesh/internal/token"
	"github.com/bfulton/sesh/internal/vault"
)

// skill runs a /sesh command through the hook and returns the output it
// hands Claude, or "" if the hook stayed silent.
func skill(t *testing.T, ev hookInput) string {
	t.Helper()
	ev.Event = "UserPromptExpansion"
	data, _ := json.Marshal(ev)
	var out strings.Builder
	if err := cmdHook(strings.NewReader(string(data)), &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() == 0 {
		return ""
	}
	var resp struct {
		HookSpecificOutput struct{ HookEventName, AdditionalContext string }
	}
	if err := json.Unmarshal([]byte(out.String()), &resp); err != nil || resp.HookSpecificOutput.HookEventName != "UserPromptExpansion" {
		t.Fatalf("bad hook output %q: %v", out.String(), err)
	}
	return resp.HookSpecificOutput.AdditionalContext
}

// stop sends a Stop event and returns the stopReason it hands Claude, if any.
func stop(t *testing.T, sessionID string) string {
	t.Helper()
	t.Setenv("SESH_HOOK_DETACHED", "1") // save in this process, not a detached copy
	data, _ := json.Marshal(hookInput{Event: "Stop", SessionID: sessionID})
	var out strings.Builder
	if err := cmdHook(strings.NewReader(string(data)), &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() == 0 {
		return ""
	}
	var resp struct {
		Continue   *bool
		StopReason string
	}
	if err := json.Unmarshal([]byte(out.String()), &resp); err != nil || resp.Continue == nil || *resp.Continue {
		t.Fatalf("bad Stop output %q: %v", out.String(), err)
	}
	return resp.StopReason
}

// /sesh's arguments are data: shell syntax in them runs nothing, and text
// shaped like a flag is an argument, not a flag. Only its five commands run.
func TestHookSkill(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SESH_HOME", filepath.Join(home, ".sesh"))
	key, _ := token.New()
	t.Setenv("SESH_KEY", key.String())
	t.Setenv("SESH_STORE", "file://"+filepath.Join(home, "store"))
	claudeEnv(t, true, true)
	tp := filepath.Join(home, "s.jsonl")
	os.WriteFile(tp, []byte(`{"type":"ai-title","aiTitle":"Token format"}
{"type":"user","message":{"role":"user","content":"Which alphabet for share tokens?"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Crockford base32."}]}}
`), 0o600)
	if _, err := sesh.Save(context.Background(), sesh.SaveOptions{
		Store: &store.Dir{Root: filepath.Join(home, "store")}, SessionID: "abcdef12-0000", TranscriptPath: tp,
		SkipWorktree: true, Vault: &vault.Vault{Key: key},
	}); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	marker := filepath.Join(home, "INJECTED")
	sesh := func(args string) string {
		return skill(t, hookInput{CommandName: "sesh:sesh", CommandArgs: args, Cwd: cwd, SessionID: "live",
			TranscriptPath: filepath.Join(home, "live.jsonl")})
	}

	if out := sesh(""); !strings.Contains(out, "vault: ") || !strings.Contains(out, "store: file://") {
		t.Errorf("bare /sesh: %q, want status", out)
	}
	if out := sesh("search crockford"); !strings.Contains(out, "Token format") {
		t.Errorf("search: %q", out)
	}
	// Searched as text: the session still matches on "crockford".
	if out := sesh("search crockford; touch " + marker + " $(touch " + marker + ") `touch " + marker + "`"); !strings.Contains(out, "Token format") {
		t.Errorf("search with shell syntax: %q, want a search for its words", out)
	}
	written := filepath.Join(home, "history.txt")
	if out := sesh("restore -o " + written + " abcdef"); !strings.Contains(out, "no saved session matches") {
		t.Errorf("restore with a flag-shaped argument: %q, want it taken as the target", out)
	}
	for _, p := range []string{marker, written} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s exists: /sesh arguments had an effect", p)
		}
	}
	// In a cloud session the restore turn gets the end of the history, and
	// the Stop hook that ends it appends the whole history, once.
	if out := sesh("restore abcdef"); !strings.Contains(out, "[sesh] Restored record") || !strings.Contains(out, "Crockford base32.") ||
		!strings.Contains(out, "When this turn ends") {
		t.Errorf("restore in a cloud session: %q, want the history's end and what follows", out)
	}
	if full := stop(t, "live"); !strings.Contains(full, "[sesh] Restored record") || !strings.Contains(full, "Crockford base32.") ||
		!strings.Contains(full, "search_messages and read_messages") {
		t.Errorf("Stop after a restore handed Claude %q, want the whole history", full)
	}
	if again := stop(t, "live"); again != "" {
		t.Errorf("a later Stop handed Claude %q, want nothing", again)
	}
	if out := sesh("key"); !strings.Contains(out, "isn't one of them") || strings.Contains(out, key.String()) {
		t.Errorf("key: %q, want a refusal", out)
	}
	if out := sesh("save"); !strings.Contains(out, "nothing to save yet") {
		t.Errorf("save before the transcript exists: %q", out)
	}
	if out := skill(t, hookInput{CommandName: "other:skill", CommandArgs: "status"}); out != "" {
		t.Errorf("another command's expansion got %q, want nothing", out)
	}
}

// Claude Code hands the model at most about 10,000 characters of hook
// context as is. A large session's restore fits, keeping where it stopped.
func TestHookSkillFitsHookContext(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SESH_HOME", filepath.Join(home, ".sesh"))
	key, _ := token.New()
	t.Setenv("SESH_KEY", key.String())
	t.Setenv("SESH_STORE", "file://"+filepath.Join(home, "store"))
	claudeEnv(t, true, true)
	var b strings.Builder
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&b, `{"type":"user","message":{"role":"user","content":"question %d %s"}}`+"\n", i, strings.Repeat("q", 100))
		fmt.Fprintf(&b, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"answer %d %s"}]}}`+"\n", i, strings.Repeat("a", 100))
	}
	b.WriteString(`{"type":"user","message":{"role":"user","content":"FINAL-STEP: wire the prefix into token.Parse"}}` + "\n")
	tp := filepath.Join(home, "big.jsonl")
	os.WriteFile(tp, []byte(b.String()), 0o600)
	if _, err := sesh.Save(context.Background(), sesh.SaveOptions{
		Store: &store.Dir{Root: filepath.Join(home, "store")}, SessionID: "bigbig00-0000", TranscriptPath: tp,
		SkipWorktree: true, Vault: &vault.Vault{Key: key},
	}); err != nil {
		t.Fatal(err)
	}
	out := skill(t, hookInput{CommandName: "sesh:sesh", CommandArgs: "restore bigbig", Cwd: t.TempDir(), SessionID: "live"})
	if n := utf8.RuneCountInString(out); n > hookContextLimit {
		t.Errorf("hook context is %d characters, over %d", n, hookContextLimit)
	}
	for _, want := range []string{"[sesh] Restored record", "FINAL-STEP"} {
		if !strings.Contains(out, want) {
			t.Errorf("hook context lacks %q", want)
		}
	}
	// The Stop hook's stopReason, which has no such limit, carries it all.
	full := stop(t, "live")
	for _, want := range []string{"question 0 ", "FINAL-STEP", "search_messages and read_messages"} {
		if !strings.Contains(full, want) {
			t.Errorf("appended history lacks %q", want)
		}
	}
}

// /sesh save replaces secrets, says how many, and remembers what it scanned.
func TestHookSaveRedacts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SESH_HOME", filepath.Join(home, ".sesh"))
	t.Setenv("SESH_REDACT", "")
	key, _ := token.New()
	t.Setenv("SESH_KEY", key.String())
	t.Setenv("SESH_STORE", "file://"+filepath.Join(home, "store"))
	// A GitHub-token-shaped string made at run time, so none sits in the source.
	fake := "ghp_" + strings.Repeat("aB3dE5gH7j", 4)[:36]
	tp := filepath.Join(home, "s.jsonl")
	line, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": "token " + fake}})
	os.WriteFile(tp, append(line, '\n'), 0o600)

	out := skill(t, hookInput{SessionID: "redact-1", TranscriptPath: tp, Cwd: home, CommandName: skillName, CommandArgs: "save"})
	if !strings.Contains(out, "1 secret replaced by a placeholder") {
		t.Errorf("/sesh save printed:\n%s", out)
	}
	if strings.Contains(out, fake) {
		t.Error("the output repeats the secret")
	}
	if _, err := os.Stat(scanCache("redact-1")); err != nil {
		t.Errorf("no scan cache: %v", err)
	}
}
