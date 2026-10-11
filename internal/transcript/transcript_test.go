package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const sample = `{"type":"queue-operation","operation":"enqueue"}
{"type":"user","message":{"role":"user","content":"Please fix the bug"},"uuid":"u1"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"secret reasoning","signature":"abc"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Looking now."},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls -la"}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"file1\nfile2"}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","is_error":true,"content":[{"type":"text","text":"denied"},{"type":"image","source":{}}]}]}}
{"type":"user","isMeta":true,"message":{"role":"user","content":"harness note"}}
{"type":"assistant","isSidechain":true,"message":{"role":"assistant","content":[{"type":"text","text":"subagent chatter"}]}}
{"type":"attachment","attachment":{"type":"date"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Fixed it."}]}}
`

func TestParse(t *testing.T) {
	got, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{
		{"user", "Please fix the bug"},
		{"assistant", "Looking now."},
		{"tool_call", `Bash {"command":"ls -la"}`},
		{"tool_result", "file1\nfile2"},
		{"tool_result", "[error] denied\n[image]"},
		{"assistant", "Fixed it."},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %#v, want %#v", i, got[i], want[i])
		}
	}
}

func TestRenderFitsBudget(t *testing.T) {
	var entries []Entry
	for i := 0; i < 200; i++ {
		entries = append(entries,
			Entry{"user", "question " + strings.Repeat("q", 50)},
			Entry{"tool_call", "Bash " + strings.Repeat("c", 3000)},
			Entry{"tool_result", "START" + strings.Repeat("é", 5000) + "END"},
			Entry{"assistant", "answer " + strings.Repeat("a", 50)})
	}
	for _, budget := range []int{1 << 20, 200_000, 50_000, 5_000} {
		s := Render(entries, "2026-10-10", "claude-code", budget)
		if len(s) > budget {
			t.Errorf("budget %d: rendered %d bytes", budget, len(s))
		}
		if !utf8.ValidString(s) {
			t.Errorf("budget %d: invalid UTF-8", budget)
		}
		if !strings.Contains(s, "answer "+strings.Repeat("a", 50)) {
			t.Errorf("budget %d: newest assistant text missing", budget)
		}
	}
	// A generous budget keeps tool output start and end and all user text.
	s := Render(entries, "d", "src", 1<<22)
	if strings.Count(s, "--- user\n") != 200 || !strings.Contains(s, "START") || !strings.Contains(s, "END") {
		t.Error("generous budget dropped content")
	}
	// A tight budget omits the oldest entries and says so.
	if s := Render(entries, "d", "src", 5_000); !strings.Contains(s[:600], "earlier entries omitted to fit]") {
		t.Error("tight budget did not mark omitted entries")
	}
}

func TestTrim(t *testing.T) {
	if got := trim("short", 100); got != "short" {
		t.Errorf("trim short = %q", got)
	}
	got := trim(strings.Repeat("x", 100)+strings.Repeat("y", 100), 20)
	if !strings.HasPrefix(got, "xxxxxxxxxx[… 180 chars …]") || !strings.HasSuffix(got, "yyyyyyyyyy") {
		t.Errorf("trim = %q", got)
	}
}

func TestProjectDir(t *testing.T) {
	got := ProjectDir("/root", "/tmp/claude-0/-home-user-sesh/a.b_c d")
	want := filepath.Join("/root", ".claude", "projects", "-tmp-claude-0--home-user-sesh-a-b-c-d")
	if got != want {
		t.Errorf("ProjectDir = %q, want %q", got, want)
	}
}

func TestWriteNative(t *testing.T) {
	home := t.TempDir()
	in := `{"type":"user","sessionId":"old","cwd":"/src","uuid":"u1","parentUuid":null,"message":{"role":"user","content":"hi"}}
{"type":"assistant","sessionId":"old","cwd":"/src","uuid":"a1","parentUuid":"u1","message":{"role":"assistant","content":[{"type":"text","text":"hello"}]}}
{"type":"last-prompt","sessionId":"old","lastPrompt":"hi","leafUuid":"a1"}
`
	path, err := WriteNative([]byte(in), Native{Home: home, Cwd: "/dst/repo", SessionID: "new-id"})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(ProjectDir(home, "/dst/repo"), "new-id.jsonl"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	data, _ := os.ReadFile(path)
	out := string(data)
	if strings.Contains(out, `"old"`) || strings.Contains(out, `"/src"`) {
		t.Errorf("old session id or cwd survived:\n%s", out)
	}
	if strings.Count(out, `"sessionId":"new-id"`) != 3 || strings.Count(out, `"cwd":"/dst/repo"`) != 2 {
		t.Errorf("rewrite incomplete:\n%s", out)
	}
	for _, keep := range []string{`"parentUuid":"u1"`, `"leafUuid":"a1"`, `"text":"hello"`} {
		if !strings.Contains(out, keep) {
			t.Errorf("lost %s:\n%s", keep, out)
		}
	}
	if _, err := WriteNative([]byte(in), Native{Home: home, Cwd: "/dst/repo", SessionID: "new-id"}); err == nil {
		t.Error("overwrote an existing session file")
	}
}

// TestRealTranscript parses and renders a real transcript when one is given.
func TestRealTranscript(t *testing.T) {
	path := os.Getenv("SESH_TEST_TRANSCRIPT")
	if path == "" {
		t.Skip("set SESH_TEST_TRANSCRIPT to a transcript path")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, e := range entries {
		kinds[e.Kind]++
	}
	t.Logf("%d bytes -> %d entries %v", len(data), len(entries), kinds)
	for _, budget := range []int{1 << 30, 300_000, 100_000} {
		s := Render(entries, "now", "claude-code", budget)
		t.Logf("budget %d -> %d bytes, omitted marker: %v", budget, len(s), strings.Contains(s[:min(len(s), 600)], "earlier entries omitted to fit]"))
		if len(s) > budget {
			t.Errorf("over budget")
		}
	}
}

func TestReadMeta(t *testing.T) {
	in := `{"type":"user","timestamp":"2026-10-10T10:00:00Z","cwd":"/w","gitBranch":"main","message":{"role":"user","content":"<command-name>x</command-name>"}}
{"type":"user","timestamp":"2026-10-10T10:00:01Z","cwd":"/w","message":{"role":"user","content":"Fix the login bug\nmore detail"}}
{"type":"assistant","timestamp":"2026-10-10T10:05:00Z","message":{"role":"assistant","content":[{"type":"text","text":"ok"}]}}
{"type":"ai-title","aiTitle":"Login bug fix"}
`
	m := ReadMeta([]byte(in))
	if m.Title != "Login bug fix" || m.Cwd != "/w" || m.Branch != "main" || m.Messages != 3 {
		t.Errorf("meta = %+v", m)
	}
	if m.Started.Format(time.RFC3339) != "2026-10-10T10:00:00Z" || m.Updated.Format(time.RFC3339) != "2026-10-10T10:05:00Z" {
		t.Errorf("times = %v %v", m.Started, m.Updated)
	}
	m = ReadMeta([]byte(in + `{"type":"custom-title","customTitle":"Mine"}` + "\n"))
	if m.Title != "Mine" {
		t.Errorf("custom title not preferred: %q", m.Title)
	}
	m = ReadMeta([]byte(strings.Split(in, "\n{\"type\":\"ai-title\"")[0]))
	if m.Title != "Fix the login bug" {
		t.Errorf("fallback title = %q", m.Title)
	}
}

// A compacted session: claude --resume follows parents from the last message
// back to the compaction boundary, so it loads the prompt after it, plus the
// prompt of the turn the compaction interrupted, which the summary names.
func TestReadResumable(t *testing.T) {
	in := `{"type":"user","uuid":"p1","parentUuid":null,"message":{"role":"user","content":"first prompt"}}
{"type":"assistant","uuid":"a1","parentUuid":"p1","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{}}]}}
{"type":"user","uuid":"r1","parentUuid":"a1","message":{"role":"user","content":[{"type":"tool_result","content":"ok"}]}}
{"type":"user","uuid":"p2","parentUuid":"r1","message":{"role":"user","content":[{"type":"text","text":"second prompt"}]}}
{"type":"user","uuid":"s1","parentUuid":"p2","isSidechain":true,"message":{"role":"user","content":"subagent task"}}
{"type":"system","subtype":"compact_boundary","uuid":"b1","parentUuid":null,"logicalParentUuid":"p2"}
{"type":"user","uuid":"c1","parentUuid":"b1","isCompactSummary":true,"summarizedTurnPromptUuids":["p2"],"message":{"role":"user","content":"This session is being continued…"}}
{"type":"user","uuid":"m1","parentUuid":"c1","isMeta":true,"message":{"role":"user","content":"harness note"}}
{"type":"user","uuid":"p3","parentUuid":"m1","message":{"role":"user","content":"third prompt"}}
{"type":"assistant","uuid":"a3","parentUuid":"p3","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}
{"type":"last-prompt","lastPrompt":"third prompt","leafUuid":"a3"}
`
	got := ReadResumable([]byte(in))
	if want := (Resumable{Compactions: 1, Prompts: 3, Loaded: 2}); got != want {
		t.Errorf("ReadResumable = %+v, want %+v", got, want)
	}
	entries, err := Parse([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Text, "continued") {
			t.Errorf("compaction summary parsed as a message: %+v", e)
		}
	}
	if got := ReadResumable([]byte(in[:strings.Index(in, `{"type":"system"`)])); got != (Resumable{Prompts: 2, Loaded: 2}) {
		t.Errorf("uncompacted: %+v", got)
	}
}

// Issue #2: a copy must not carry the original's live bindings, and leaving
// out a message must not break the chain claude --resume follows.
func TestWriteNativeDropsBindings(t *testing.T) {
	home := t.TempDir()
	in := `{"type":"user","sessionId":"old","uuid":"u1","parentUuid":null,"message":{"role":"user","content":"hi"}}
{"type":"system","subtype":"bridge_status","sessionId":"old","uuid":"b1","parentUuid":"u1","url":"https://claude.ai/code/session_X"}
{"type":"system","subtype":"bridge_status","sessionId":"old","uuid":"b2","parentUuid":"b1","url":"https://claude.ai/code/session_X"}
{"type":"assistant","sessionId":"old","uuid":"a1","parentUuid":"b2","message":{"role":"assistant","content":[{"type":"text","text":"hello"}]}}
{"type":"system","subtype":"compact_boundary","sessionId":"old","uuid":"c1","parentUuid":null,"logicalParentUuid":"b2"}
{"type":"bridge-session","sessionId":"old","bridgeSessionId":"cse_X","lastSequenceNum":14500}
{"type":"worktree-state","sessionId":"old","worktreeSession":{"worktreePath":"/orig/wt"}}
{"type":"relocated","sessionId":"old","relocatedCwd":"/orig"}
{"type":"dev-mods","sessionId":"old"}
{"type":"artifact-comment-monitor","sessionId":"old"}
{"type":"artifact-autoreact-ledger","sessionId":"old"}
{"type":"custom-title","sessionId":"old","customTitle":"kept"}
`
	path, err := WriteNative([]byte(in), Native{Home: home, Cwd: "/dst", SessionID: "new"})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	out := string(data)
	for _, gone := range []string{"cse_X", "session_X", "bridge-session", "worktree-state", "relocated", "dev-mods", "artifact-", `"b1"`, `"b2"`} {
		if strings.Contains(out, gone) {
			t.Errorf("%s survived:\n%s", gone, out)
		}
	}
	for _, keep := range []string{`"parentUuid":"u1"`, `"logicalParentUuid":"u1"`, `"customTitle":"kept"`, `"text":"hello"`} {
		if !strings.Contains(out, keep) {
			t.Errorf("missing %s:\n%s", keep, out)
		}
	}
	if n := strings.Count(out, "\n"); n != 4 {
		t.Errorf("wrote %d records, want 4:\n%s", n, out)
	}
}

func TestWriteNativeRefusesUnsafeFiles(t *testing.T) {
	for _, p := range []string{"../escape", "tool-results/../../escape", "/etc/x", "tool-results", "tool-results/", "other/x", `tool-results\..\x`} {
		home := t.TempDir()
		_, err := WriteNative([]byte(`{"type":"user"}`+"\n"), Native{Home: home, Cwd: "/dst", SessionID: "new", Files: map[string][]byte{p: []byte("x")}})
		if err == nil {
			t.Errorf("accepted session file %q", p)
		}
	}
	home := t.TempDir()
	if _, err := WriteNative([]byte(`{"type":"user"}`+"\n"), Native{Home: home, Cwd: "/dst", SessionID: "new", Files: map[string][]byte{"tool-results/a/b.txt": []byte("x")}}); err != nil {
		t.Errorf("refused a nested tool output: %v", err)
	}
}
