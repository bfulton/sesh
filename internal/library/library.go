package library

import (
	"context"
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

	"github.com/bfulton/sesh/internal/sesh"
	"github.com/bfulton/sesh/internal/store"
	"github.com/bfulton/sesh/internal/token"
	"github.com/bfulton/sesh/internal/transcript"
	"github.com/bfulton/sesh/internal/vault"
)

// maxToolText caps how much of each tool call or result is kept locally;
// the full session stays in the store.
const maxToolText = 4000

// cached is one session's local copy.
type cached struct {
	Entry   vault.Entry        `json:"entry"`
	Entries []transcript.Entry `json:"entries"`
}

// Library is the local copy of a vault and its index.
type Library struct {
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
	local, err := loadCache(dir)
	if err != nil {
		return nil, err
	}
	lib := &Library{sessions: local}
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

// pull downloads sessions that are new or saved since they were cached.
func (l *Library) pull(ctx context.Context, st store.Store, dir string, entries []vault.Entry) error {
	var todo []vault.Entry
	for _, e := range entries {
		if c, ok := l.sessions[e.ID()]; !ok || e.SavedAt.After(c.Entry.SavedAt) {
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
	entries, err := transcript.Parse(a.Transcript)
	if err != nil {
		return nil, err
	}
	for i := range entries {
		if k := entries[i].Kind; (k == "tool_call" || k == "tool_result") && len(entries[i].Text) > maxToolText {
			entries[i].Text = transcript.Trim(entries[i].Text, maxToolText)
		}
	}
	return &cached{Entry: e, Entries: entries}, nil
}

func writeCached(dir string, c *cached) error {
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(c.Entry.ID()))
	p := filepath.Join(dir, hex.EncodeToString(sum[:16])+".json")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
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
