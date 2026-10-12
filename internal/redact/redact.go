// Package redact finds secrets in what a session saves, with the rules of
// gitleaks (https://github.com/gitleaks/gitleaks), and replaces each with a
// placeholder that names its rule, such as [REDACTED:github-pat].
package redact

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/rs/zerolog"
	"github.com/zricethezav/gitleaks/v8/config"
	"github.com/zricethezav/gitleaks/v8/detect"
	"github.com/zricethezav/gitleaks/v8/logging"
)

// gitleaks' regular expressions slow down more than linearly with the size
// of what they scan: a 667 KB transcript took 1.4 s line by line and 12 s in
// 1 MB pieces. So text is scanned in windows of about chunkSize bytes, each
// starting up to overlap bytes before its chunk, so that a secret across a
// chunk boundary, such as a private key, is whole in one window.
const (
	chunkSize = 16 << 10
	overlap   = 8 << 10

	// decodeDepth is gitleaks' own default: it also finds secrets inside
	// base64, hex and percent encoding.
	decodeDepth = 5

	// version changes whenever sesh would redact the same input differently,
	// so that content scanned before is scanned again.
	version = "1"
)

// A Redactor scans text with gitleaks' default rules.
type Redactor struct {
	d  *detect.Detector
	fp [16]byte
}

var (
	once      sync.Once
	shared    *Redactor
	sharedErr error
)

// New returns the Redactor. gitleaks keeps its configuration in global
// state, so there is one per process.
func New() (*Redactor, error) {
	once.Do(func() {
		// gitleaks logs findings, secrets included, at debug level; and the
		// MCP server's stdout is its protocol.
		logging.Logger = zerolog.Nop()
		d, err := detect.NewDetectorDefaultConfig()
		if err != nil {
			sharedErr = fmt.Errorf("loading gitleaks rules: %w", err)
			return
		}
		d.MaxDecodeDepth = decodeDepth
		sum := sha256.Sum256([]byte(fmt.Sprintf("sesh-redact %s %d %d %d\n%s",
			version, decodeDepth, chunkSize, overlap, config.DefaultConfig)))
		shared = &Redactor{d: d}
		copy(shared.fp[:], sum[:])
	})
	return shared, sharedErr
}

// Fingerprint identifies the rules and settings. Content scanned clean under
// one fingerprint needs scanning again under another.
func (r *Redactor) Fingerprint() [16]byte { return r.fp }

// A target is a secret to replace: its text wherever it occurs, or, for a
// secret gitleaks found by decoding, the encoded span of the scanned text.
type target struct {
	rule       string
	secret     string
	start, end int
}

// Windows remembers the windows scanned clean.
type Windows interface {
	Skip(window string) bool // known clean, or not to be scanned now
	Clean(window string)     // scanned and found clean
}

// scan returns the secrets in s. wc may be nil.
func (r *Redactor) scan(ctx context.Context, s string, wc Windows) ([]target, error) {
	var out []target
	for a := 0; a < len(s); {
		w := windowStart(s, a)
		b := blockEnd(s, w, chunkEnd(s, a))
		window := s[w:b]
		a = b
		if wc != nil && wc.Skip(window) {
			continue
		}
		ts, err := r.detect(ctx, window, w)
		if err != nil {
			return nil, err
		}
		if len(ts) == 0 && wc != nil {
			wc.Clean(window)
		}
		out = append(out, ts...)
	}
	return out, nil
}

// detect runs gitleaks on one window, which starts at offset base of the
// scanned text.
func (r *Redactor) detect(ctx context.Context, window string, base int) ([]target, error) {
	fs := r.d.DetectContext(ctx, detect.Fragment{Raw: window})
	// gitleaks stops early, without an error, when ctx is done.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out []target
	for _, f := range fs {
		switch {
		case f.Secret == "":
		case strings.Contains(window, f.Secret):
			out = append(out, target{rule: f.RuleID, secret: f.Secret})
		default:
			// Found decoded: its location is the encoded text's.
			start, end, ok := span(window, f.StartLine, f.StartColumn, f.EndLine, f.EndColumn)
			if !ok {
				start, end = lineStart(window, f.StartLine), lineEnd(window, f.EndLine)
			}
			out = append(out, target{rule: f.RuleID, start: base + start, end: base + end})
		}
	}
	return out, nil
}

// span turns a gitleaks location into byte offsets. gitleaks counts lines
// from 0 and, on every line but the first, columns from the line's newline.
func span(s string, startLine, startCol, endLine, endCol int) (int, int, bool) {
	start := newlineBefore(s, startLine) + startCol - 1
	end := newlineBefore(s, endLine) + endCol
	if startLine < 0 || endLine < 0 || start < 0 || end > len(s) || start >= end {
		return 0, 0, false
	}
	return start, end, true
}

// newlineBefore returns the offset of the newline that ends line n-1, or 0
// for line 0.
func newlineBefore(s string, n int) int {
	off := 0
	for i := 0; i < n; i++ {
		j := strings.IndexByte(s[off:], '\n')
		if j < 0 {
			return len(s)
		}
		off += j
		if i < n-1 {
			off++
		}
	}
	return off
}

func lineStart(s string, n int) int {
	if n <= 0 {
		return 0
	}
	return min(newlineBefore(s, n)+1, len(s))
}

func lineEnd(s string, n int) int {
	off := lineStart(s, n)
	if j := strings.IndexByte(s[off:], '\n'); j >= 0 {
		return off + j
	}
	return len(s)
}

// chunkEnd returns where the chunk starting at a ends: at the first line end
// after chunkSize bytes, or at chunkSize bytes if that line is long.
func chunkEnd(s string, a int) int {
	if len(s)-a <= chunkSize {
		return len(s)
	}
	b := a + chunkSize
	if j := strings.IndexByte(s[b:min(len(s), b+chunkSize)], '\n'); j >= 0 {
		return b + j + 1
	}
	return runeStart(s, b)
}

// maxBlock bounds how far a chunk grows to hold a whole PEM block.
const maxBlock = 1 << 20

// blockEnd moves a chunk end b past the end of a PEM block, such as a
// private key, that begins in the window from w but doesn't end in it.
// gitleaks' private-key rule has no length limit, and a key longer than the
// overlap, such as a PGP key with subkeys, would otherwise be whole in no
// window.
func blockEnd(s string, w, b int) int {
	win := strings.ToLower(s[w:b])
	i := strings.LastIndex(win, "-----begin")
	if i < 0 || strings.Contains(win[i:], "-----end") {
		return b
	}
	rest := strings.ToLower(s[b:min(len(s), b+maxBlock)])
	j := strings.Index(rest, "-----end")
	if j < 0 {
		return b
	}
	end := b + j
	if k := strings.IndexByte(s[end:], '\n'); k >= 0 {
		return end + k + 1
	}
	return len(s)
}

// windowStart returns where the window for the chunk at a starts: up to
// overlap bytes earlier, at a line start if there is one in reach.
func windowStart(s string, a int) int {
	w := max(0, a-overlap)
	if w == 0 {
		return 0
	}
	if j := strings.IndexByte(s[w:a], '\n'); j >= 0 {
		return w + j + 1
	}
	return runeStart(s, w)
}

// runeStart moves i back to the start of a UTF-8 sequence.
func runeStart(s string, i int) int {
	for i > 0 && i < len(s) && s[i]&0xC0 == 0x80 {
		i--
	}
	return i
}

// Placeholder returns what replaces a secret found by rule. It keeps the
// secret's line breaks, so line-based formats such as patches keep their
// shape.
func Placeholder(rule, secret string) string {
	return "[REDACTED:" + rule + "]" + strings.Repeat("\n", strings.Count(secret, "\n"))
}

// apply replaces targets found in prefix+s within s, and returns how many
// placeholders it wrote. Spans that reach into prefix are clipped to s.
func apply(prefix, s string, ts []target) (string, int) {
	n := 0
	var spans, literals []target
	for _, t := range ts {
		if t.secret != "" {
			literals = append(literals, t)
			continue
		}
		t.start = max(0, t.start-len(prefix))
		t.end -= len(prefix)
		if t.end > t.start {
			spans = append(spans, t)
		}
	}
	// Spans from the end, so the offsets of the rest stay valid.
	sort.Slice(spans, func(i, j int) bool { return spans[i].start > spans[j].start })
	limit := len(s)
	for _, t := range spans {
		if t.end > limit {
			continue // overlaps one already replaced
		}
		s = s[:t.start] + Placeholder(t.rule, s[t.start:t.end]) + s[t.end:]
		limit = t.start
		n++
	}
	// Longest first, so a secret that contains another goes whole; of rules
	// that found the same secret, a specific one names it.
	sort.SliceStable(literals, func(i, j int) bool {
		a, b := literals[i], literals[j]
		if len(a.secret) != len(b.secret) {
			return len(a.secret) > len(b.secret)
		}
		return a.rule != genericRule && b.rule == genericRule
	})
	for _, t := range literals {
		if k := strings.Count(s, t.secret); k > 0 {
			s = strings.ReplaceAll(s, t.secret, Placeholder(t.rule, t.secret))
			n += k
		}
	}
	return s, n
}

// maxRounds bounds how often redacted text is scanned again. Replacing one
// secret can expose another that overlapped it.
const maxRounds = 3

// redact returns s with its secrets replaced, scanning prefix+s so that a
// rule can see the context prefix gives, such as a JSON key. It returns how
// many placeholders it wrote.
func (r *Redactor) redact(ctx context.Context, prefix, s string, wc Windows) (string, int, error) {
	n := 0
	for round := 0; ; round++ {
		ts, err := r.scan(ctx, prefix+s, wc)
		if err != nil {
			return "", 0, err
		}
		if len(ts) == 0 {
			return s, n, nil
		}
		next, k := apply(prefix, s, ts)
		if next == s {
			// What gitleaks found is in prefix, or is a placeholder.
			return s, n, nil
		}
		n += k
		s = next
		if round == maxRounds-1 {
			break
		}
	}
	// Still finding secrets: replace each line that holds one.
	ts, err := r.scan(ctx, prefix+s, wc)
	if err != nil {
		return "", 0, err
	}
	full := prefix + s
	var lines []target
	for _, t := range ts {
		start, end := t.start, t.end
		if t.secret != "" {
			i := strings.Index(full, t.secret)
			if i < 0 {
				continue
			}
			start, end = i, i+len(t.secret)
		}
		start = strings.LastIndexByte(full[:start], '\n') + 1
		if j := strings.IndexByte(full[end:], '\n'); j >= 0 {
			end += j
		} else {
			end = len(full)
		}
		lines = append(lines, target{rule: t.rule, start: start, end: end})
	}
	s, k := apply(prefix, s, lines)
	return s, n + k, nil
}

// genericRule finds secrets by the words around them, so other rules often
// find the same secret too.
const genericRule = "generic-api-key"
