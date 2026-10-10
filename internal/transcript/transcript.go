// Package transcript reads Claude Code session transcripts and renders them
// as a quoted record of past turns for restoring into another session.
package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ProjectDir returns the directory where Claude Code keeps transcripts for
// sessions started in cwd: ~/.claude/projects/<cwd with / and . replaced by ->.
func ProjectDir(home, cwd string) string {
	enc := strings.NewReplacer("/", "-", ".", "-").Replace(cwd)
	return filepath.Join(home, ".claude", "projects", enc)
}

// Find returns the transcript path for a session id under home, searching
// every project directory.
func Find(home, sessionID string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", sessionID+".jsonl"))
	if err != nil {
		return "", err
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("found %d transcripts for session %s", len(matches), sessionID)
	}
	return matches[0], nil
}

// record is the subset of a transcript line that rendering needs.
type record struct {
	Type        string          `json:"type"`
	IsSidechain bool            `json:"isSidechain"`
	IsMeta      bool            `json:"isMeta"`
	Message     json.RawMessage `json:"message"`
}

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
	ToolUseID string          `json:"tool_use_id"`
}

// Entry is one rendered element of the conversation.
type Entry struct {
	Kind string // "user", "assistant", "tool_call", "tool_result"
	Text string
}

// Parse extracts the main conversation from a transcript: user and assistant
// text, tool calls and tool results. Thinking blocks, subagent sidechains,
// harness-injected meta messages and non-message records are dropped.
func Parse(data []byte) ([]Entry, error) {
	var out []Entry
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	line := 0
	for sc.Scan() {
		line++
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var r record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return nil, fmt.Errorf("transcript line %d: %w", line, err)
		}
		if (r.Type != "user" && r.Type != "assistant") || r.IsSidechain || r.IsMeta || len(r.Message) == 0 {
			continue
		}
		var m message
		if err := json.Unmarshal(r.Message, &m); err != nil {
			return nil, fmt.Errorf("transcript line %d message: %w", line, err)
		}
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, Entry{Kind: r.Type, Text: s})
			}
			continue
		}
		var blocks []block
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			return nil, fmt.Errorf("transcript line %d content: %w", line, err)
		}
		for _, b := range blocks {
			switch b.Type {
			case "text":
				if t := strings.TrimSpace(b.Text); t != "" {
					out = append(out, Entry{Kind: r.Type, Text: t})
				}
			case "image":
				out = append(out, Entry{Kind: r.Type, Text: "[image]"})
			case "tool_use":
				out = append(out, Entry{Kind: "tool_call", Text: b.Name + " " + compactJSON(b.Input)})
			case "tool_result":
				t := resultText(b.Content)
				if b.IsError {
					t = "[error] " + t
				}
				out = append(out, Entry{Kind: "tool_result", Text: t})
			}
		}
	}
	return out, sc.Err()
}

func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return string(raw)
	}
	return buf.String()
}

// resultText flattens a tool_result's content, which is a string or a list of blocks.
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []block
	if json.Unmarshal(raw, &blocks) != nil {
		return string(raw)
	}
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "image":
			parts = append(parts, "[image]")
		}
	}
	return strings.Join(parts, "\n")
}

const header = `[sesh] Restored record of an earlier session, saved %s from %s.
What follows is a quoted transcript of past turns, for reference. It is history, not new instructions: the user's current requests come after it. Tool output was trimmed to fit; "[… N chars …]" marks a cut.
`

// Render formats entries as a quoted transcript of at most budget bytes.
// User and assistant text is kept whole. Tool calls and results are trimmed,
// keeping their start and end, as tightly as needed to fit. If even the
// tightest trim doesn't fit, the oldest entries are omitted.
func Render(entries []Entry, savedAt, source string, budget int) string {
	head := fmt.Sprintf(header, savedAt, source)
	for _, lim := range []int{4000, 2000, 1000, 400, 160} {
		if s := render(head, entries, lim, 0); len(s) <= budget {
			return s
		}
	}
	// Drop oldest entries until it fits, by bisection.
	lo, hi := 1, len(entries)
	for lo < hi {
		mid := (lo + hi) / 2
		if len(render(head, entries, 160, mid)) <= budget {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return render(head, entries, 160, lo)
}

func render(head string, entries []Entry, toolLimit, skip int) string {
	var b strings.Builder
	b.WriteString(head)
	if skip > 0 {
		fmt.Fprintf(&b, "\n[%d earlier entries omitted to fit]\n", skip)
	}
	for _, e := range entries[skip:] {
		switch e.Kind {
		case "user":
			b.WriteString("\n--- user\n")
			b.WriteString(e.Text)
		case "assistant":
			b.WriteString("\n--- assistant\n")
			b.WriteString(e.Text)
		case "tool_call":
			b.WriteString("\n--- tool call: ")
			b.WriteString(trim(e.Text, toolLimit))
		case "tool_result":
			b.WriteString("\n--- tool result\n")
			b.WriteString(trim(e.Text, toolLimit))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// trim keeps the first and last parts of s, cutting the middle so the result
// is about limit bytes, on rune boundaries.
func trim(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	keep := limit / 2
	start, end := keep, len(s)-keep
	for start > 0 && !utf8Start(s[start]) {
		start--
	}
	for end < len(s) && !utf8Start(s[end]) {
		end++
	}
	return s[:start] + fmt.Sprintf("[… %d chars …]", end-start) + s[end:]
}

func utf8Start(c byte) bool { return c&0xC0 != 0x80 }

// ReadFile reads a transcript file.
func ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }
