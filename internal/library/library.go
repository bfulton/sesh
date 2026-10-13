package library

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/bfulton/sesh/internal/archive"
	"github.com/bfulton/sesh/internal/sesh"
	"github.com/bfulton/sesh/internal/store"
	"github.com/bfulton/sesh/internal/token"
	"github.com/bfulton/sesh/internal/transcript"
	"github.com/bfulton/sesh/internal/vault"
)

// maxToolText caps how much of each tool call or result is kept locally;
// the full session stays in the store.
const maxToolText = 4000

// cacheFormat changes when the way sessions are parsed changes, so that the
// next pull re-reads sessions cached the old way.
const cacheFormat = 2

// cached is one session's local copy.
type cached struct {
	Format  int                `json:"format"`
	Entry   vault.Entry        `json:"entry"`
	Entries []transcript.Entry `json:"entries"`
}

// Library is the local copy of a vault and its index.
type Library struct {
	Dir      string        // the cache folder
	Version  string        // its version when read; see ReadVersion
	Entries  []vault.Entry // most recently active first
	sessions map[string]*cached
	index    *Index
	Skipped  int // catalog entries that couldn't be read
}

// Open loads the library from cacheDir. With pull, it first brings the
// cache up to date with the store: new and changed sessions are downloaded
// and decrypted. Without pull it works offline from the cache.
func Open(ctx context.Context, st store.Store, v *vault.Vault, cacheDir string, pull bool) (*Library, error) {
	dir := filepath.Join(cacheDir, v.ID())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// Read before the cache, so that a session added while it's read
	// shows as a new version.
	version := ReadVersion(dir)
	local, err := loadCache(dir)
	if err != nil {
		return nil, err
	}
	lib := &Library{Dir: dir, Version: version, sessions: local}
	if pull {
		entries, bad, err := v.List(ctx, st)
		if err != nil {
			return nil, fmt.Errorf("reading catalog: %w", err)
		}
		lib.Skipped = bad
		if err := lib.pull(ctx, st, dir, entries); err != nil {
			return nil, err
		}
	}
	for _, c := range lib.sessions {
		lib.Entries = append(lib.Entries, c.Entry)
	}
	sort.Slice(lib.Entries, func(i, j int) bool { return lib.Entries[i].Active().After(lib.Entries[j].Active()) })
	lib.buildIndex()
	return lib, nil
}

func loadCache(dir string) (map[string]*cached, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	out := map[string]*cached{}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var c cached
		if json.Unmarshal(data, &c) != nil || c.Entry.Session == "" {
			os.Remove(p) // a damaged cache file is just re-downloaded
			continue
		}
		out[c.Entry.ID()] = &c
	}
	return out, nil
}

// pull downloads sessions that are new or saved since they were cached, and
// any cached under another token than the catalog's: the catalog decides
// what a vault session id holds.
func (l *Library) pull(ctx context.Context, st store.Store, dir string, entries []vault.Entry) error {
	var todo []vault.Entry
	for _, e := range entries {
		if c, ok := l.sessions[e.ID()]; !ok || c.Format != cacheFormat || c.Entry.Token != e.Token || e.SavedAt.After(c.Entry.SavedAt) {
			todo = append(todo, e)
		}
	}
	var mu sync.Mutex
	var firstErr error
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, e := range todo {
		wg.Add(1)
		sem <- struct{}{}
		go func(e vault.Entry) {
			defer wg.Done()
			defer func() { <-sem }()
			c, err := fetch(ctx, st, e)
			if err == nil {
				err = writeCached(dir, c)
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("session %s: %w", e.ID(), err)
				}
				return
			}
			l.sessions[e.ID()] = c
		}(e)
	}
	wg.Wait()
	return firstErr
}

func fetch(ctx context.Context, st store.Store, e vault.Entry) (*cached, error) {
	tok, err := token.Parse(e.Token)
	if err != nil {
		return nil, err
	}
	a, err := sesh.Fetch(ctx, st, tok)
	if err != nil {
		return nil, err
	}
	return newCached(e, a.Transcript)
}

func newCached(e vault.Entry, data []byte) (*cached, error) {
	entries, err := transcript.Parse(data)
	if err != nil {
		return nil, err
	}
	for i := range entries {
		if k := entries[i].Kind; (k == "tool_call" || k == "tool_result") && len(entries[i].Text) > maxToolText {
			entries[i].Text = transcript.Trim(entries[i].Text, maxToolText)
		}
	}
	return &cached{Format: cacheFormat, Entry: e, Entries: entries}, nil
}

// Remember adds a session restored from archive a, saved under tok, to the
// library in cacheDir, so it can be searched and read like the vault's own
// sessions. A session restored from someone else's share token is in no
// catalog the library pulls. A copy cached from the same save or a later
// one is kept as it is.
//
// An archive names its own session id, and anyone can make one that names
// another's. So a session cached under another token is never replaced
// here, and one this adds is replaced by the next pull if the vault's
// catalog holds that id under its own token.
func Remember(cacheDir string, v *vault.Vault, tok token.Token, a *archive.Archive) error {
	dir := filepath.Join(cacheDir, v.ID())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	e := sesh.CatalogEntry(a, tok, "")
	if data, err := os.ReadFile(cachePath(dir, e.ID())); err == nil {
		var old cached
		if json.Unmarshal(data, &old) == nil && old.Entry.Session != "" {
			if old.Entry.Token != e.Token {
				return fmt.Errorf("session %s is already in the library under another share token", e.ID())
			}
			if old.Format == cacheFormat && !e.SavedAt.After(old.Entry.SavedAt) {
				return nil
			}
		}
	}
	c, err := newCached(e, a.Transcript)
	if err != nil {
		return err
	}
	return writeCached(dir, c)
}

func cachePath(dir, id string) string {
	sum := sha256.Sum256([]byte(id))
	return filepath.Join(dir, hex.EncodeToString(sum[:16])+".json")
}

func writeCached(dir string, c *cached) error {
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	p := cachePath(dir, c.Entry.ID())
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		return err
	}
	return newVersion(dir)
}

// versionFile holds a random value that changes whenever a session is
// written to the cache, so that a reader, such as the MCP server, sees
// another process's restore at once. Modification times don't serve: they
// come from a coarse clock, and a write just after a read can carry an
// earlier time than the read's.
const versionFile = "version"

// ReadVersion returns the version of the cache folder dir; it's "" until a
// session is first written.
func ReadVersion(dir string) string {
	data, _ := os.ReadFile(filepath.Join(dir, versionFile))
	return string(data)
}

func newVersion(dir string) error {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	p := filepath.Join(dir, versionFile)
	if err := os.WriteFile(p+".tmp", []byte(hex.EncodeToString(b[:])), 0o600); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}

func (l *Library) buildIndex() {
	var docs []Doc
	for _, e := range l.Entries {
		c := l.sessions[e.ID()]
		for i, en := range c.Entries {
			docs = append(docs, Doc{Session: e.ID(), Seq: i, Kind: en.Kind, Text: en.Text})
		}
		// The title is searchable too, as a pseudo user message.
		if e.Title != "" {
			docs = append(docs, Doc{Session: e.ID(), Seq: -1, Kind: "user", Text: e.Title})
		}
	}
	l.index = NewIndex(docs)
}

// Hit is a search result: the best-matching message in a session.
type Hit struct {
	Entry   vault.Entry
	Seq     int    // message position; -1 for a title match
	Kind    string // message kind
	Snippet string
	Score   float64
	More    int // other matching messages in the same session
}

// Search returns the best-matching sessions, each with its best message.
func (l *Library) Search(query string, limit int) []Hit {
	matches := l.index.Search(query)
	bySession := map[string]*Hit{}
	var order []string
	for _, m := range matches {
		d := l.index.docs[m.Doc]
		if h, ok := bySession[d.Session]; ok {
			h.More++
			continue
		}
		bySession[d.Session] = &Hit{
			Entry: l.sessions[d.Session].Entry, Seq: d.Seq, Kind: d.Kind,
			Snippet: Snippet(d.Text, query, 220), Score: m.Score,
		}
		order = append(order, d.Session)
	}
	var out []Hit
	for _, s := range order {
		out = append(out, *bySession[s])
		if len(out) == limit {
			break
		}
	}
	return out
}

// SearchMessages returns the best-matching individual messages across all
// sessions, so an agent can jump straight to the relevant part.
func (l *Library) SearchMessages(query string, limit int) []Hit {
	var out []Hit
	for _, m := range l.index.Search(query) {
		d := l.index.docs[m.Doc]
		if d.Seq < 0 {
			continue
		}
		out = append(out, Hit{
			Entry: l.sessions[d.Session].Entry, Seq: d.Seq, Kind: d.Kind,
			Snippet: Snippet(d.Text, query, 400), Score: m.Score,
		})
		if len(out) == limit {
			break
		}
	}
	return out
}

// Listed is one message from ListMessages, with where it sits.
type Listed struct {
	Session vault.Entry // its session
	Seq     int         // message number, as read_messages counts
	transcript.Entry
}

// ListMessages lists every message of kind ("" for every kind) in the
// sessions keep accepts (nil keeps all), most recently active session first
// and in order within a session. It returns up to limit of them from offset
// on, and how many there are in all, so an agent can count and page through
// a whole history instead of sampling search results.
func (l *Library) ListMessages(kind string, keep func(vault.Entry) bool, offset, limit int) ([]Listed, int) {
	var out []Listed
	total := 0
	for _, e := range l.Entries {
		if keep != nil && !keep(e) {
			continue
		}
		for i, en := range l.sessions[e.ID()].Entries {
			if kind != "" && en.Kind != kind {
				continue
			}
			if total >= offset && len(out) < limit {
				out = append(out, Listed{Session: e, Seq: i, Entry: en})
			}
			total++
		}
	}
	return out, total
}

// Messages returns up to n messages of a session, by entry ID, starting at
// start, with the session's total message count.
func (l *Library) Messages(sessionID string, start, n int) ([]transcript.Entry, int, error) {
	c, ok := l.sessions[sessionID]
	if !ok {
		return nil, 0, errors.New("session not in the library")
	}
	total := len(c.Entries)
	start = max(0, min(start, total))
	end := min(total, start+max(n, 0))
	return c.Entries[start:end], total, nil
}

// Handle returns a short name for e that Find resolves to e alone.
func (l *Library) Handle(e vault.Entry) string { return vault.Handle(l.Entries, e) }

// Find returns entries matching an id prefix or title words.
func (l *Library) Find(q string) []vault.Entry { return vault.Find(l.Entries, q) }

// Conversation returns a session's messages, by entry ID (vault.Entry.ID).
func (l *Library) Conversation(sessionID string) ([]transcript.Entry, error) {
	c, ok := l.sessions[sessionID]
	if !ok {
		return nil, errors.New("session not in the library")
	}
	return c.Entries, nil
}

// Render returns a session, by entry ID, as a quoted record of past turns
// within budget bytes.
func (l *Library) Render(sessionID string, budget int) (string, error) {
	c, ok := l.sessions[sessionID]
	if !ok {
		return "", errors.New("session not in the library")
	}
	source := "session " + c.Entry.Session
	if c.Entry.Title != "" {
		source += " (" + c.Entry.Title + ")"
	}
	return transcript.Render(c.Entries, c.Entry.SavedAt.Format(time.RFC3339), source, budget), nil
}
