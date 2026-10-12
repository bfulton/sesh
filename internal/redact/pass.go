package redact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// A Pass redacts the files of one save. It skips content its cache knows is
// clean, and once its deadline passes it stops scanning, only measuring what
// is left, so that a save with a time limit resumes where the last stopped.
type Pass struct {
	r        *Redactor
	cache    *Cache
	ctx      context.Context
	deadline time.Time

	// Redacted counts the secrets replaced in each file, by name.
	Redacted map[string]int

	scanned, left int64
}

// NewPass starts a pass. A zero deadline sets no time limit; cache may be nil.
func NewPass(ctx context.Context, r *Redactor, cache *Cache, deadline time.Time) *Pass {
	if cache == nil {
		cache = &Cache{}
	}
	cache.reset(r.fp)
	return &Pass{r: r, cache: cache, ctx: ctx, deadline: deadline, Redacted: map[string]int{}}
}

// Incomplete is the error of a pass that ran out of time. What it scanned is
// in its cache, so the next pass carries on from there.
type Incomplete struct {
	Scanned, Left int64 // bytes
}

func (e *Incomplete) Error() string {
	return fmt.Sprintf("still scanning for secrets (%s of %s scanned); the next save carries on",
		mb(e.Scanned), mb(e.Scanned+e.Left))
}

func mb(n int64) string { return fmt.Sprintf("%.1f MB", float64(n)/1e6) }

// Finish saves the cache and reports whether the pass covered everything.
// It returns an *Incomplete error if not: nothing it redacted may be saved.
func (p *Pass) Finish() error {
	complete := p.left == 0
	if err := p.cache.save(complete); err != nil {
		return fmt.Errorf("saving the scan cache: %w", err)
	}
	if !complete {
		return &Incomplete{Scanned: p.scanned, Left: p.left}
	}
	return nil
}

// outOfTime reports whether the deadline has passed. Content not scanned
// because of it counts toward the bytes left.
func (p *Pass) outOfTime() bool {
	return !p.deadline.IsZero() && time.Now().After(p.deadline)
}

// Skip implements Windows: a window known clean is skipped; past the
// deadline, every other window is counted as left and skipped too, which
// leaves the pass incomplete.
func (p *Pass) Skip(w string) bool {
	if p.cache.has(hash('t', w)) {
		return true
	}
	if p.outOfTime() {
		p.left += int64(len(w))
		return true
	}
	p.scanned += int64(len(w))
	return false
}

// Clean implements Windows.
func (p *Pass) Clean(w string) { p.cache.add(hash('t', w)) }

// Text redacts a text file. Files that aren't UTF-8 are left as they are.
func (p *Pass) Text(name string, data []byte) ([]byte, error) {
	if !utf8.Valid(data) {
		return data, nil
	}
	out, n, err := p.r.redact(p.ctx, "", string(data), p)
	if err != nil {
		return nil, err
	}
	p.count(name, n)
	return []byte(out), nil
}

func (p *Pass) count(name string, n int) {
	if n > 0 {
		p.Redacted[name] += n
	}
}

// JSONL redacts a JSONL file, such as a transcript, line by line. Each
// string is scanned decoded, after its key, which gitleaks' rules use as
// context: in the raw line, a secret just after an escaped newline has no
// word boundary before it, and rules that need one miss it. A line with a
// secret is encoded again with the secret replaced; every other line stays
// byte for byte.
func (p *Pass) JSONL(name string, data []byte) ([]byte, error) {
	var out bytes.Buffer
	out.Grow(len(data))
	total := 0
	for len(data) > 0 {
		line := data
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			line = data[:i+1]
		}
		data = data[len(line):]
		body := bytes.TrimRight(line, "\r\n")
		red, n, err := p.jsonLine(body)
		if err != nil {
			return nil, err
		}
		total += n
		if n == 0 {
			out.Write(line)
		} else {
			out.Write(red)
			out.Write(line[len(body):])
		}
	}
	p.count(name, total)
	return out.Bytes(), nil
}

func (p *Pass) jsonLine(line []byte) ([]byte, int, error) {
	if len(bytes.TrimSpace(line)) == 0 {
		return line, 0, nil
	}
	h := hash('j', string(line))
	if p.cache.has(h) {
		return line, 0, nil
	}
	if p.outOfTime() {
		p.left += int64(len(line))
		return line, 0, nil
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil || dec.More() || !utf8.Valid(line) {
		// Not one JSON value: scan it as text.
		out, n, err := p.r.redact(p.ctx, "", string(line), p)
		return []byte(out), n, err
	}
	p.scanned += int64(len(line))
	n := 0
	v, err := p.walk(v, "", &n)
	if err != nil {
		return nil, 0, err
	}
	if n == 0 {
		p.cache.add(h)
		return line, 0, nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, 0, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), n, nil
}

// walk redacts the strings in a decoded JSON value. key is the object key
// the value is under, if any.
func (p *Pass) walk(v any, key string, n *int) (any, error) {
	switch x := v.(type) {
	case string:
		if x == "" {
			return x, nil
		}
		prefix := ""
		if key != "" {
			prefix = key + ": "
		}
		out, k, err := p.r.redact(p.ctx, prefix, x, nil)
		*n += k
		return out, err
	case []any:
		for i := range x {
			y, err := p.walk(x[i], key, n)
			if err != nil {
				return nil, err
			}
			x[i] = y
		}
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			y, err := p.walk(x[k], k, n)
			if err != nil {
				return nil, err
			}
			x[k] = y
		}
	}
	return v, nil
}

// Patch redacts the lines a git patch adds. Lines it keeps or removes are in
// the base commit, which a restore needs anyway, and redacting them would
// stop the patch from applying. Binary patches are left as they are.
func (p *Pass) Patch(name string, data []byte) ([]byte, error) {
	if bytes.Contains(data, []byte("\nGIT binary patch\n")) || !utf8.Valid(data) {
		return data, nil
	}
	lines := strings.SplitAfter(string(data), "\n")
	var added []int
	var text strings.Builder
	inHunk := false
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "@@"):
			inHunk = true
		case strings.HasPrefix(l, "diff --git "):
			inHunk = false
		case inHunk && strings.HasPrefix(l, "+"):
			added = append(added, i)
			text.WriteString(l[1:])
		}
	}
	if len(added) == 0 {
		return data, nil
	}
	out, n, err := p.r.redact(p.ctx, "", text.String(), p)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return data, nil
	}
	red := strings.SplitAfter(out, "\n")
	if red[len(red)-1] == "" {
		red = red[:len(red)-1]
	}
	if len(red) != len(added) {
		return nil, errors.New("redacting " + name + " changed its line count")
	}
	for k, i := range added {
		lines[i] = "+" + red[k]
	}
	p.count(name, n)
	return []byte(strings.Join(lines, "")), nil
}

func hash(kind byte, s string) [16]byte {
	h := sha256.New()
	h.Write([]byte{kind})
	h.Write([]byte(s))
	var out [16]byte
	copy(out[:], h.Sum(nil))
	return out
}

// A Cache remembers, as hashes, content scanned and found clean, so that
// saving a session again scans only what changed. It never holds content.
type Cache struct {
	path string
	fp   [16]byte
	old  map[[16]byte]bool // read from path
	used map[[16]byte]bool // known clean in this pass
}

// OpenCache reads the cache at path. A missing or unreadable cache is empty.
func OpenCache(path string) *Cache {
	c := &Cache{path: path, old: map[[16]byte]bool{}}
	data, err := os.ReadFile(path)
	if err != nil || len(data) < 16 || len(data)%16 != 0 {
		return c
	}
	copy(c.fp[:], data)
	for i := 16; i < len(data); i += 16 {
		var h [16]byte
		copy(h[:], data[i:])
		c.old[h] = true
	}
	return c
}

// reset empties a cache made under other rules.
func (c *Cache) reset(fp [16]byte) {
	if c.fp != fp {
		c.fp, c.old = fp, map[[16]byte]bool{}
	}
	if c.used == nil {
		c.used = map[[16]byte]bool{}
	}
}

func (c *Cache) has(h [16]byte) bool {
	if c.old[h] || c.used[h] {
		c.used[h] = true
		return true
	}
	return false
}

func (c *Cache) add(h [16]byte) { c.used[h] = true }

// save writes the cache. After a complete pass it keeps only what that pass
// used, so it tracks the session's current content; after an incomplete one
// it keeps everything.
func (c *Cache) save(complete bool) error {
	if c.path == "" {
		return nil
	}
	keep := c.used
	if !complete {
		for h := range c.old {
			keep[h] = true
		}
	}
	buf := make([]byte, 0, 16*(len(keep)+1))
	buf = append(buf, c.fp[:]...)
	for h := range keep {
		buf = append(buf, h[:]...)
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(c.path), ".scan-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(buf); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), c.path)
}
