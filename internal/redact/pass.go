package redact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// A Pass redacts the files of one save. Content its cache knows is skipped:
// clean content as it is, content with secrets as the cache's redacted copy.
// Once its deadline passes it stops scanning, only measuring what is left,
// so that a save with a time limit resumes where the last stopped.
type Pass struct {
	r        *Redactor
	cache    *Cache
	ctx      context.Context
	deadline time.Time

	// Redacted counts the placeholders written in each file, by name.
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
	if p.cache.clean(hash('t', w)) {
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
func (p *Pass) Clean(w string) { p.cache.addClean(hash('t', w)) }

// Text redacts a text file. Files that aren't UTF-8 are left as they are.
func (p *Pass) Text(name string, data []byte) ([]byte, error) {
	if !utf8.Valid(data) {
		return data, nil
	}
	return p.cached(name, hash('T', string(data)), data, func() ([]byte, int, error) {
		out, n, err := p.r.redact(p.ctx, "", string(data), p)
		return []byte(out), n, err
	})
}

// cached returns the cache's redacted copy of data, whose hash is h, if it
// has one, and otherwise redacts it with f. A redacted copy is cached only
// when all of data was scanned.
func (p *Pass) cached(name string, h [16]byte, data []byte, f func() ([]byte, int, error)) ([]byte, error) {
	if out, n, ok := p.cache.redacted(h); ok {
		p.count(name, n)
		return out, nil
	}
	left := p.left
	out, n, err := f()
	if err != nil {
		return nil, err
	}
	if n > 0 && p.left == left {
		p.cache.addRedacted(h, out, n)
	}
	p.count(name, n)
	return out, nil
}

func (p *Pass) count(name string, n int) {
	if n > 0 {
		p.Redacted[name] += n
	}
}

// JSONL redacts a JSONL file, such as a transcript, line by line. Each
// string is scanned decoded, after its key, which gitleaks' rules use as
// context: in the raw line, a secret just after an escaped newline has no
// word boundary before it, and rules that need one miss it. Every member of
// an object is scanned, a repeated key included. A line with a secret is
// encoded again, members in their order, with the secret replaced; every
// other line stays byte for byte. A line that isn't one JSON value, such as the end of a
// file still being written, is left out: scanning it raw could miss a
// secret, and a line being written is whole by the next save.
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
		if errors.Is(err, errNotJSON) {
			continue
		}
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

var errNotJSON = errors.New("not one JSON value")

func (p *Pass) jsonLine(line []byte) ([]byte, int, error) {
	if len(bytes.TrimSpace(line)) == 0 {
		return line, 0, nil
	}
	h := hash('j', string(line))
	if p.cache.clean(h) {
		return line, 0, nil
	}
	if out, n, ok := p.cache.redacted(h); ok {
		return out, n, nil
	}
	if p.outOfTime() {
		p.left += int64(len(line))
		return line, 0, nil
	}
	if !utf8.Valid(line) {
		return nil, 0, errNotJSON
	}
	v, err := decodeJSON(line)
	if err != nil {
		return nil, 0, errNotJSON
	}
	p.scanned += int64(len(line))
	left, n := p.left, 0
	if v, err = p.walk(v, "", &n); err != nil {
		return nil, 0, err
	}
	whole := p.left == left
	if n == 0 {
		if whole {
			p.cache.addClean(h)
		}
		return line, 0, nil
	}
	var buf bytes.Buffer
	if err := encodeJSON(&buf, v); err != nil {
		return nil, 0, err
	}
	if whole {
		p.cache.addRedacted(h, buf.Bytes(), n)
	}
	return buf.Bytes(), n, nil
}

// stringWindows checks the deadline between the windows of a decoded JSON
// string. It caches nothing: the line's hash is cached once all of it is
// clean.
type stringWindows struct{ p *Pass }

func (w stringWindows) Skip(s string) bool {
	if w.p.outOfTime() {
		w.p.left += int64(len(s))
		return true
	}
	return false
}

func (stringWindows) Clean(string) {}

// binarySource reports whether a JSON object is the source of an image or
// PDF, such as {"type":"base64","media_type":"image/png","data":...}. Its
// data is compressed binary, in which no rule finds text, and scanning it
// costs: a run of encoded text is scanned whole. Other media types, such as
// text/plain, are scanned.
func binarySource(o object) bool {
	t, _ := o.get("type").(string)
	mt, _ := o.get("media_type").(string)
	return t == "base64" && (strings.HasPrefix(mt, "image/") || mt == "application/pdf")
}

// walk redacts the strings in a decoded JSON value, object keys included.
// key is the object key the value is under, if any.
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
		out, k, err := p.r.redact(p.ctx, prefix, x, stringWindows{p})
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
	case object:
		binary := binarySource(x)
		for i, m := range x {
			if m.key == "data" && binary {
				continue
			}
			y, err := p.walk(m.val, m.key, n)
			if err != nil {
				return nil, err
			}
			x[i].val = y
			nk, kn, err := p.r.redact(p.ctx, "", m.key, stringWindows{p})
			if err != nil {
				return nil, err
			}
			if kn == 0 {
				continue
			}
			*n += kn
			// A placeholder another key already has gets a number, so the
			// object doesn't gain a repeated key whose earlier value a
			// reader would drop.
			base := nk
			for j := 2; x.has(nk); j++ {
				nk = fmt.Sprintf("%s %d", base, j)
			}
			x[i].key = nk
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
	return p.cached(name, hash('P', string(data)), data, func() ([]byte, int, error) {
		return p.patch(name, data)
	})
}

func (p *Pass) patch(name string, data []byte) ([]byte, int, error) {
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
		return data, 0, nil
	}
	out, n, err := p.r.redact(p.ctx, "", text.String(), p)
	if err != nil {
		return nil, 0, err
	}
	if n == 0 {
		return data, 0, nil
	}
	red := strings.SplitAfter(out, "\n")
	if red[len(red)-1] == "" {
		red = red[:len(red)-1]
	}
	if len(red) != len(added) {
		return nil, 0, errors.New("redacting " + name + " changed its line count")
	}
	for k, i := range added {
		lines[i] = "+" + red[k]
	}
	return []byte(strings.Join(lines, "")), n, nil
}

func hash(kind byte, s string) [16]byte {
	h := sha256.New()
	h.Write([]byte{kind})
	h.Write([]byte(s))
	var out [16]byte
	copy(out[:], h.Sum(nil))
	return out
}

// A Cache remembers what of a session was scanned, so that saving it again
// scans only what changed. Content found clean is kept as its hash alone.
// Content with secrets is kept as its hash and a redacted copy, in a file
// of its own beside the cache, so that it isn't scanned again either; the
// copy holds placeholders, never the secrets.
type Cache struct {
	path string
	fp   [16]byte
	old  map[[16]byte]int // read from path: placeholders, 0 when clean
	used map[[16]byte]int // known in this pass

	copies map[[16]byte][]byte // redacted copies, when there is no path
}

// OpenCache reads the cache at path. A missing or unreadable cache is empty.
func OpenCache(path string) *Cache {
	c := &Cache{path: path, old: map[[16]byte]int{}}
	data, err := os.ReadFile(path)
	if err != nil || len(data) < 16 || (len(data)-16)%20 != 0 {
		return c
	}
	copy(c.fp[:], data)
	for i := 16; i < len(data); i += 20 {
		var h [16]byte
		copy(h[:], data[i:])
		c.old[h] = int(binary.BigEndian.Uint32(data[i+16:]))
	}
	return c
}

// reset empties a cache made under other rules.
func (c *Cache) reset(fp [16]byte) {
	if c.fp != fp {
		c.fp, c.old = fp, map[[16]byte]int{}
	}
	if c.used == nil {
		c.used = map[[16]byte]int{}
	}
	if c.copies == nil {
		c.copies = map[[16]byte][]byte{}
	}
}

func (c *Cache) lookup(h [16]byte) (int, bool) {
	if n, ok := c.used[h]; ok {
		return n, true
	}
	if n, ok := c.old[h]; ok {
		c.used[h] = n
		return n, true
	}
	return 0, false
}

// clean reports whether content with hash h was scanned and found clean.
func (c *Cache) clean(h [16]byte) bool {
	n, ok := c.lookup(h)
	return ok && n == 0
}

// redacted returns the redacted copy of content with hash h, and how many
// placeholders it holds, if the cache has one.
func (c *Cache) redacted(h [16]byte) ([]byte, int, bool) {
	n, ok := c.lookup(h)
	if !ok || n == 0 {
		return nil, 0, false
	}
	if data, ok := c.copies[h]; ok {
		return data, n, true
	}
	if c.path != "" {
		if data, err := os.ReadFile(c.copyPath(h)); err == nil {
			return data, n, true
		}
	}
	delete(c.used, h) // the copy is gone: scan again
	return nil, 0, false
}

func (c *Cache) addClean(h [16]byte) { c.used[h] = 0 }

// addRedacted keeps the redacted copy of content with hash h. If the copy
// can't be written, the content is scanned again next time.
func (c *Cache) addRedacted(h [16]byte, out []byte, n int) {
	if c.path == "" {
		c.copies[h] = out
	} else if writeFile(c.copyPath(h), out) != nil {
		return
	}
	c.used[h] = n
}

func (c *Cache) copyPath(h [16]byte) string {
	return filepath.Join(c.path+".d", hex.EncodeToString(h[:]))
}

// save writes the cache. After a complete pass it keeps only what that pass
// used, so it tracks the session's current content, and removes redacted
// copies no longer used; after an incomplete one it keeps everything.
func (c *Cache) save(complete bool) error {
	if c.path == "" {
		return nil
	}
	keep := c.used
	if !complete {
		for h, n := range c.old {
			if _, ok := keep[h]; !ok {
				keep[h] = n
			}
		}
	}
	buf := make([]byte, 0, 16+20*len(keep))
	buf = append(buf, c.fp[:]...)
	for h, n := range keep {
		buf = append(buf, h[:]...)
		buf = binary.BigEndian.AppendUint32(buf, uint32(n))
	}
	if err := writeFile(c.path, buf); err != nil {
		return err
	}
	if complete {
		entries, _ := os.ReadDir(c.path + ".d")
		for _, e := range entries {
			var h [16]byte
			if b, err := hex.DecodeString(e.Name()); err == nil && len(b) == 16 {
				copy(h[:], b)
				if keep[h] > 0 {
					continue
				}
			}
			os.Remove(filepath.Join(c.path+".d", e.Name()))
		}
	}
	return nil
}

// writeFile replaces a file at once, readable only by its owner.
func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".scan-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}
