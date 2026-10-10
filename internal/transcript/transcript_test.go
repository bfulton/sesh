package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	got := ProjectDir("/root", "/tmp/claude-0/-home-user-sesh/a.b")
	want := filepath.Join("/root", ".claude", "projects", "-tmp-claude-0--home-user-sesh-a-b")
	if got != want {
		t.Errorf("ProjectDir = %q, want %q", got, want)
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
