// Package transcript reads Claude Code session transcripts and renders them
// as a quoted record of past turns for restoring into another session.
package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// ProjectDir returns the directory where Claude Code keeps transcripts for
// sessions started in cwd: ~/.claude/projects/<cwd with every character
// other than an ASCII letter or digit replaced by ->.
func ProjectDir(home, cwd string) string {
	enc := []byte(cwd)
	for i, c := range enc {
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9') {
			enc[i] = '-'
		}
	}
	return filepath.Join(home, ".claude", "projects", string(enc))
}

// boundTypes are metadata records that bind a session to something live:
// its Remote Control bridge, its worktree, the folder it moved to, its
// dev-mods folder, the artifacts whose comments it watches. Claude Code
// applies them to whatever session id they carry, so a copy that kept them
// would take over the original's bindings; Claude Code's own fork drops the
// same state.
var boundTypes = map[string]bool{
	"bridge-session":            true,
	"worktree-state":            true,
	"relocated":                 true,
	"dev-mods":                  true,
	"artifact-comment-monitor":  true,
	"artifact-autoreact-ledger": true,
}

// boundSubtypes are system messages that only point at a live binding, such
// as the original's Remote Control link.
var boundSubtypes = map[string]bool{
	"bridge_status": true,
}

// Native is a copy of a session to write as a new Claude Code session.
type Native struct {
	Home      string // home directory holding .claude/projects
	Cwd       string // working directory the copy belongs to
	SessionID string // the copy's session id

	// SideDir is the folder beside the original transcript where Claude Code
	// kept its large tool outputs and subagent transcripts; Files holds them
	// by path within it. They are written beside the copy, references to
	// SideDir are pointed at the copy's folder, and subagent transcripts are
	// rewritten like the main one.
	SideDir string
	Files   map[string][]byte
}

// WriteNative writes a transcript as a new Claude Code session for cwd, so
// `claude --resume <sessionID>` there continues it with its real turns. Every
// message keeps its content; only its session id, working directory and
// references to the session's own folder change. Records binding the
// original to live state (boundTypes) are left out, so the copy gets its own
// Remote Control bridge, worktree and the rest; a message left out is
// unlinked from the chain, its children taking its parent. It returns the
// path of the transcript written.
func WriteNative(data []byte, n Native) (string, error) {
	pdir := ProjectDir(n.Home, n.Cwd)
	side := filepath.Join(pdir, n.SessionID)
	repoint, err := repointer(n.SideDir, side)
	if err != nil {
		return "", err
	}
	main, err := n.rewrite(data, repoint)
	if err != nil {
		return "", err
	}
	files := map[string][]byte{}
	for p, b := range n.Files {
		if !validSideFile(p) {
			return "", fmt.Errorf("session file %q: unexpected path", p)
		}
		switch {
		case strings.HasPrefix(p, "subagents/") && strings.HasSuffix(p, ".jsonl"):
			if b, err = n.rewrite(b, repoint); err != nil {
				return "", fmt.Errorf("session file %s: %w", p, err)
			}
		case strings.HasPrefix(p, "subagents/"):
			b = repoint(b)
		}
		files[p] = b
	}
	if err := os.MkdirAll(pdir, 0o700); err != nil {
		return "", err
	}
	tpath := filepath.Join(pdir, n.SessionID+".jsonl")
	f, err := os.OpenFile(tpath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	fail := func(err error) (string, error) {
		f.Close()
		os.Remove(tpath)
		return "", err
	}
	// Side files first: once the transcript exists, the session can be
	// resumed, and it should find everything it refers to.
	for p, b := range files {
		dst := filepath.Join(side, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return fail(err)
		}
		if err := os.WriteFile(dst, b, 0o600); err != nil {
			return fail(err)
		}
	}
	if _, err := f.Write(main); err != nil {
		return fail(err)
	}
	return tpath, f.Close()
}

// SideDirs are the parts of a session's folder that belong to its history:
// large tool outputs and subagent transcripts. Others, such as a cloud
// session's event pointer, bind the original to live state.
var SideDirs = []string{"tool-results", "subagents"}

func validSideFile(p string) bool {
	if path.Clean(p) != p || path.IsAbs(p) || strings.Contains(p, "\\") {
		return false
	}
	for _, d := range SideDirs {
		if strings.HasPrefix(p, d+"/") {
			return true
		}
	}
	return false
}

// repointer returns a function replacing references to the folder from
// with to, as they appear inside JSON strings. Without from it changes
// nothing.
func repointer(from, to string) (func([]byte) []byte, error) {
	if from == "" {
		return func(b []byte) []byte { return b }, nil
	}
	enc := func(s string) ([]byte, error) {
		b, err := json.Marshal(s + "/")
		if err != nil {
			return nil, err
		}
		return b[1 : len(b)-1], nil
	}
	old, err := enc(from)
	if err != nil {
		return nil, err
	}
	nw, err := enc(to)
	if err != nil {
		return nil, err
	}
	return func(b []byte) []byte { return bytes.ReplaceAll(b, old, nw) }, nil
}

// rewrite gives every record of a transcript the copy's session id and
// working directory and repoints references to the session's folder,
// leaving out records bound to the original.
func (n Native) rewrite(data []byte, repoint func([]byte) []byte) ([]byte, error) {
	sid, err := json.Marshal(n.SessionID)
	if err != nil {
		return nil, err
	}
	dir, err := json.Marshal(n.Cwd)
	if err != nil {
		return nil, err
	}
	// The parent of each message left out, already relinked itself, so one
	// lookup takes a child past any run of left-out ancestors.
	dropped := map[string]json.RawMessage{}
	relink := func(r map[string]json.RawMessage, key string) {
		var id string
		if json.Unmarshal(r[key], &id) == nil {
			if up, ok := dropped[id]; ok {
				r[key] = up
			}
		}
	}
	var out bytes.Buffer
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var r map[string]json.RawMessage
		if err := json.Unmarshal(repoint(line), &r); err != nil {
			return nil, fmt.Errorf("transcript: %w", err)
		}
		var kind struct{ Type, Subtype, UUID string }
		json.Unmarshal(line, &kind)
		relink(r, "parentUuid")
		relink(r, "logicalParentUuid")
		if boundTypes[kind.Type] || kind.Type == "system" && boundSubtypes[kind.Subtype] {
			if kind.UUID != "" {
				p := r["parentUuid"]
				if p == nil {
					p = json.RawMessage("null")
				}
				dropped[kind.UUID] = p
			}
			continue
		}
		if _, ok := r["sessionId"]; ok {
			r["sessionId"] = sid
		}
		if _, ok := r["cwd"]; ok {
			r["cwd"] = dir
		}
		b, err := json.Marshal(r)
		if err != nil {
			return nil, err
		}
		out.Write(b)
		out.WriteByte('\n')
	}
	return out.Bytes(), sc.Err()
}

// Resumable says how much of a transcript `claude --resume` brings back.
// Claude Code loads a session by following parent links back from its
// latest message, and a compaction starts a new chain: everything before the
// last compaction stays in the file but is loaded only as its summary.
type Resumable struct {
	Compactions int // compactions in the transcript
	Prompts     int // prompts typed in the main conversation
	Loaded      int // of those, prompts on the chain claude --resume loads
}

// ReadResumable measures a transcript as Resumable describes. Unparseable
// lines are skipped.
func ReadResumable(data []byte) Resumable {
	var res Resumable
	parent := map[string]string{}
	prompt := map[string]bool{}
	carried := map[string][]string{}
	leaf := ""
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var r record
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		if r.Type == "system" && r.Subtype == "compact_boundary" {
			res.Compactions++
		}
		if r.UUID == "" || r.IsSidechain {
			continue
		}
		parent[r.UUID] = r.ParentUUID
		if r.IsSummary {
			carried[r.UUID] = r.TurnPrompts
		}
		if r.Type == "user" || r.Type == "assistant" {
			leaf = r.UUID
		}
		if r.Type == "user" && !r.IsMeta && !r.IsSummary && isPrompt(r.Message) {
			prompt[r.UUID] = true
			res.Prompts++
		}
	}
	seen := map[string]bool{}
	loaded := map[string]bool{}
	for id := leaf; id != "" && !seen[id]; id = parent[id] {
		seen[id] = true
		for _, p := range append([]string{id}, carried[id]...) {
			if prompt[p] && !loaded[p] {
				loaded[p] = true
				res.Loaded++
			}
		}
	}
	return res
}

// isPrompt reports whether a user message is something typed, rather than
// tool results returned to the model.
func isPrompt(raw json.RawMessage) bool {
	var m message
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return strings.TrimSpace(s) != ""
	}
	var blocks []block
	if json.Unmarshal(m.Content, &blocks) != nil {
		return false
	}
	for _, b := range blocks {
		if b.Type == "tool_result" {
			return false
		}
	}
	return len(blocks) > 0
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

// record is the subset of a transcript line that sesh reads.
type record struct {
	Type        string `json:"type"`
	IsSidechain bool   `json:"isSidechain"`
	IsMeta      bool   `json:"isMeta"`
	IsSummary   bool   `json:"isCompactSummary"`
	Subtype     string `json:"subtype"`
	UUID        string `json:"uuid"`
	ParentUUID  string `json:"parentUuid"`
	// On a compaction summary: prompts of the turn it interrupted, which
	// Claude Code shows after the summary.
	TurnPrompts []string        `json:"summarizedTurnPromptUuids"`
	Message     json.RawMessage `json:"message"`
	Timestamp   string          `json:"timestamp"`
	Cwd         string          `json:"cwd"`
	GitBranch   string          `json:"gitBranch"`
	CustomTitle string          `json:"customTitle"`
	AITitle     string          `json:"aiTitle"`
	Relocated   string          `json:"relocatedCwd"`
}

// Meta is what a transcript says about its session.
type Meta struct {
	Title    string    // the user's title, else Claude Code's generated title, else the first prompt
	Cwd      string    // working directory now: of the latest message, or a later move
	Branch   string    // git branch of the latest message that names one
	Started  time.Time // first timestamp
	Updated  time.Time // last timestamp
	Messages int       // user and assistant messages in the main conversation
}

// ReadMeta extracts session metadata from a transcript. Unparseable lines
// are skipped: metadata is best effort.
func ReadMeta(data []byte) Meta {
	var m Meta
	var custom, ai, first string
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var r record
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		switch r.Type {
		case "custom-title":
			custom = r.CustomTitle
		case "ai-title":
			ai = r.AITitle
		case "relocated":
			// Claude Code moved the session to another folder.
			if r.Relocated != "" {
				m.Cwd = r.Relocated
			}
		}
		if ts, err := time.Parse(time.RFC3339Nano, r.Timestamp); err == nil {
			if m.Started.IsZero() {
				m.Started = ts
			}
			m.Updated = ts
		}
		if (r.Type != "user" && r.Type != "assistant") || r.IsSidechain || r.IsMeta {
			continue
		}
		if r.Cwd != "" {
			m.Cwd = r.Cwd
		}
		if r.GitBranch != "" {
			m.Branch = r.GitBranch
		}
		var msg message
		if json.Unmarshal(r.Message, &msg) != nil {
			continue
		}
		var s string
		if json.Unmarshal(msg.Content, &s) == nil {
			if r.Type == "user" && first == "" && !strings.HasPrefix(strings.TrimSpace(s), "<") {
				first = strings.TrimSpace(s)
			}
			m.Messages++
			continue
		}
		var blocks []block
		if json.Unmarshal(msg.Content, &blocks) != nil {
			continue
		}
		for _, b := range blocks {
			if b.Type == "text" {
				m.Messages++
				break
			}
		}
	}
	switch {
	case custom != "":
		m.Title = custom
	case ai != "":
		m.Title = ai
	default:
		m.Title = firstLine(first, 80)
	}
	return m
}

func firstLine(s string, max int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > max {
		cut := max
		for cut > 0 && !utf8Start(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
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
// harness-injected meta messages, compaction summaries (the transcript keeps
// what they summarize) and non-message records are dropped.
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
		if (r.Type != "user" && r.Type != "assistant") || r.IsSidechain || r.IsMeta || r.IsSummary || len(r.Message) == 0 {
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
// keeping their start and end, as tightly as needed to fit, down to a
// one-line stub: the conversation holds the decisions, and the tool output is
// still in the saved session. If even stubs don't fit, the oldest entries are
// omitted, and if the newest alone doesn't fit, it is trimmed, keeping its
// start and end.
func Render(entries []Entry, savedAt, source string, budget int) string {
	head := fmt.Sprintf(header, savedAt, source)
	for _, lim := range []int{4000, 2000, 1000, 400, 160, stubs} {
		if s := render(head, entries, lim, 0); len(s) <= budget {
			return s
		}
	}
	// Drop oldest entries until it fits, by bisection.
	lo, hi := 1, len(entries)
	for lo < hi {
		mid := (lo + hi) / 2
		if len(render(head, entries, stubs, mid)) <= budget {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	if lo < len(entries) || len(entries) == 0 {
		return render(head, entries, stubs, lo)
	}
	// Not even the newest entry fits whole: trim it to the room left.
	last := len(entries) - 1
	kept := append([]Entry(nil), entries...)
	kept[last].Text = ""
	room := budget - len(render(head, kept, stubs, last)) - len("[… 0000000000 chars …]")
	kept[last].Text = trim(entries[last].Text, max(room, 0))
	return render(head, kept, stubs, last)
}

// stubs, as a tool limit, cuts each tool call to the start of its first line
// and each tool result to its length.
const stubs = 0

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
			if toolLimit == stubs {
				line, _, more := strings.Cut(e.Text, "\n")
				b.WriteString(trim(line, 80))
				if more {
					b.WriteString(" …")
				}
			} else {
				b.WriteString(trim(e.Text, toolLimit))
			}
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
// Trim keeps the first and last parts of s, cutting the middle so the result
// is about limit bytes, on rune boundaries.
func Trim(s string, limit int) string { return trim(s, limit) }

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
