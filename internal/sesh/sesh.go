// Package sesh implements save and restore: a session's transcript and
// working copy, sealed under a token and kept in a store.
package sesh

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bfulton/sesh/internal/archive"
	"github.com/bfulton/sesh/internal/redact"
	"github.com/bfulton/sesh/internal/store"
	"github.com/bfulton/sesh/internal/token"
	"github.com/bfulton/sesh/internal/transcript"
	"github.com/bfulton/sesh/internal/vault"
	"github.com/bfulton/sesh/internal/worktree"
)

// DefaultBudget is the default size of a rendered history, in bytes: about
// 45,000 tokens of a typical transcript. The full session stays in the store,
// for sesh's search tools to read.
const DefaultBudget = 100_000

// SaveOptions says what to save and where.
type SaveOptions struct {
	Store          store.Store
	SessionID      string // source session id
	TranscriptPath string // source transcript file
	Dir            string // working directory; its git repo, if any, is captured
	SkipWorktree   bool   // save the conversation only, e.g. for a session that isn't live

	// With a vault, the share token derives from the vault key and the
	// session is added to the vault's catalog. Without one, a random token
	// is kept in TokenDir between saves.
	Vault    *vault.Vault
	TokenDir string
	Host     string // recorded in the catalog entry
	Now      time.Time

	// Secrets in the save are replaced by placeholders, unless KeepSecrets
	// is set. ScanCache is a file that remembers content scanned clean, so a
	// later save of the session scans only what changed. ScanBudget, if set,
	// limits the time spent scanning: a save out of time uploads nothing and
	// fails with *redact.Incomplete, and the next save carries on.
	KeepSecrets bool
	ScanCache   string
	ScanBudget  time.Duration
}

// SaveResult describes a completed save.
type SaveResult struct {
	Token    token.Token
	Manifest archive.Manifest
	Turns    int    // user and assistant messages in the transcript
	Worktree string // why the working copy was not captured, if it wasn't
	Pushed   bool   // whether the base commit is on a remote
	Skipped  int    // session files left out to keep the save under maxSessionFiles
	Redacted int    // secrets replaced by placeholders
	Entry    *vault.Entry
}

// Save captures and uploads a session. A session keeps one token across
// saves, so its token always restores the latest save.
func Save(ctx context.Context, o SaveOptions) (*SaveResult, error) {
	data, err := os.ReadFile(o.TranscriptPath)
	if err != nil {
		return nil, fmt.Errorf("reading transcript: %w", err)
	}
	if _, err := transcript.Parse(data); err != nil {
		return nil, err
	}
	meta := transcript.ReadMeta(data)
	var tok token.Token
	if o.Vault != nil {
		if o.SessionID == "" {
			return nil, errors.New("no session id")
		}
		tok = o.Vault.SessionToken(o.SessionID)
	} else if tok, err = sessionToken(o.TokenDir, o.SessionID); err != nil {
		return nil, err
	}
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	a := &archive.Archive{
		Manifest: archive.Manifest{
			Source:  "claude-code",
			Session: o.SessionID,
			SavedAt: o.Now.UTC(),
		},
		Transcript: data,
	}
	res := &SaveResult{Token: tok, Turns: meta.Messages}
	if a.Memory, err = readMemory(filepath.Join(filepath.Dir(o.TranscriptPath), "memory")); err != nil {
		return nil, fmt.Errorf("reading memory folder: %w", err)
	}
	side := strings.TrimSuffix(o.TranscriptPath, ".jsonl")
	if a.Files, res.Skipped, err = readSessionFiles(side); err != nil {
		return nil, fmt.Errorf("reading session folder: %w", err)
	}
	if len(a.Files) > 0 {
		if a.Manifest.SessionDir, err = filepath.Abs(side); err != nil {
			return nil, err
		}
	}
	if o.SkipWorktree {
		res.Worktree = "not captured for this save"
	} else if c, err := worktree.Take(o.Dir); err != nil {
		res.Worktree = err.Error()
	} else {
		a.Manifest.Repo, a.Manifest.Branch, a.Manifest.Base = c.Repo, c.Branch, c.Base
		a.Manifest.Deleted = c.Deleted
		a.Patches = c.Patches
		res.Pushed = c.Pushed
	}
	if !o.KeepSecrets {
		if err := redactArchive(ctx, a, o); err != nil {
			return nil, err
		}
		for _, n := range a.Manifest.Redacted {
			res.Redacted += n
		}
	}
	blob, err := archive.Seal(a, tok.Key())
	if err != nil {
		return nil, err
	}
	if err := o.Store.Put(ctx, vault.ArchiveName(tok), blob); err != nil {
		return nil, fmt.Errorf("uploading: %w", err)
	}
	res.Manifest = a.Manifest
	for p := range a.Patches {
		res.Manifest.Patches = append(res.Manifest.Patches, p)
	}
	if o.Vault != nil {
		e := CatalogEntry(a, tok, o.Host)
		// The catalog is written after the archive, so an entry never points
		// at an archive that isn't there.
		if err := o.Vault.Put(ctx, o.Store, e); err != nil {
			return nil, fmt.Errorf("updating catalog: %w", err)
		}
		res.Entry = &e
	}
	return res, nil
}

// redactArchive replaces the secrets in everything a save carries.
func redactArchive(ctx context.Context, a *archive.Archive, o SaveOptions) error {
	r, err := redact.New()
	if err != nil {
		return err
	}
	var cache *redact.Cache
	if o.ScanCache != "" {
		cache = redact.OpenCache(o.ScanCache)
	}
	var deadline time.Time
	if o.ScanBudget > 0 {
		deadline = time.Now().Add(o.ScanBudget)
	}
	p := redact.NewPass(ctx, r, cache, deadline)
	if a.Transcript, err = p.JSONL(archive.TranscriptName, a.Transcript); err != nil {
		return err
	}
	for _, rel := range sortedKeys(a.Files) {
		if strings.HasSuffix(rel, ".jsonl") {
			a.Files[rel], err = p.JSONL(archive.FileName(rel), a.Files[rel])
		} else {
			a.Files[rel], err = p.Text(archive.FileName(rel), a.Files[rel])
		}
		if err != nil {
			return err
		}
	}
	for _, rel := range sortedKeys(a.Memory) {
		if a.Memory[rel], err = p.Text(archive.MemoryName(rel), a.Memory[rel]); err != nil {
			return err
		}
	}
	for _, path := range sortedKeys(a.Patches) {
		if a.Patches[path], err = p.Patch(archive.PatchName(path), a.Patches[path]); err != nil {
			return err
		}
	}
	if err := p.Finish(); err != nil {
		return err
	}
	if len(p.Redacted) > 0 {
		a.Manifest.Redacted = p.Redacted
	}
	return nil
}

func sortedKeys(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// CatalogEntry describes an archive, saved under tok, as a vault catalog
// entry.
func CatalogEntry(a *archive.Archive, tok token.Token, host string) vault.Entry {
	meta := transcript.ReadMeta(a.Transcript)
	m := a.Manifest
	e := vault.Entry{
		Session: m.Session, Source: m.Source, Title: meta.Title,
		Cwd: meta.Cwd, Repo: m.Repo, Branch: meta.Branch, Started: meta.Started, Updated: meta.Updated,
		SavedAt: m.SavedAt, Messages: meta.Messages, Bytes: len(a.Transcript),
		Token: tok.String(), Host: host,
	}
	if m.Branch != "" {
		e.Branch = m.Branch
	}
	return e
}

// sessionToken returns the token saved for a session, creating one if needed.
func sessionToken(dir, sessionID string) (token.Token, error) {
	if sessionID == "" || strings.ContainsAny(sessionID, `/\`) || sessionID == "." || sessionID == ".." {
		return token.Token{}, fmt.Errorf("invalid session id %q", sessionID)
	}
	p := filepath.Join(dir, sessionID)
	if b, err := os.ReadFile(p); err == nil {
		return token.Parse(string(b))
	} else if !errors.Is(err, os.ErrNotExist) {
		return token.Token{}, err
	}
	tok, err := token.New()
	if err != nil {
		return token.Token{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return token.Token{}, err
	}
	if err := os.WriteFile(p, []byte(tok.String()+"\n"), 0o600); err != nil {
		return token.Token{}, err
	}
	return tok, nil
}

// RestoreOptions says what to restore and where.
type RestoreOptions struct {
	Store  store.Store
	Token  token.Token
	Dir    string // target working directory
	Budget int    // maximum rendered history size in bytes
	Now    time.Time

	// Native also writes the history as a new Claude Code session under
	// Home, so `claude --resume` continues it with real turns. It suits a
	// local CLI; in a hosted session the file would never be read.
	Native bool
	Home   string
}

// RestoreResult describes a completed restore.
type RestoreResult struct {
	Manifest archive.Manifest
	Turns    int    // user and assistant messages restored
	Rendered string // the history, ready to place in the model's context
	Branch   string // branch the working copy was restored onto, if any
	Worktree string // why the working copy was not restored, if it wasn't
	Archive  *archive.Archive

	NativeSession string               // id of the native session written, if any
	NativePath    string               // its transcript file
	NativeErr     string               // why it couldn't be written, if Native was asked for
	Resumable     transcript.Resumable // how much of it claude --resume loads

	entries         []transcript.Entry
	savedAt, source string
}

// RenderWithin renders the history again, within budget bytes.
func (r *RestoreResult) RenderWithin(budget int) string {
	return transcript.Render(r.entries, r.savedAt, r.source, budget)
}

// Restore downloads a saved session, restores its working copy into Dir and
// renders its history. A working-copy problem does not stop the history from
// being restored; it is reported in Worktree instead.
func Restore(ctx context.Context, o RestoreOptions) (*RestoreResult, error) {
	a, err := Fetch(ctx, o.Store, o.Token)
	if err != nil {
		return nil, err
	}
	entries, err := transcript.Parse(a.Transcript)
	if err != nil {
		return nil, err
	}
	if o.Budget <= 0 {
		o.Budget = DefaultBudget
	}
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	m := a.Manifest
	res := &RestoreResult{Manifest: m, Archive: a}
	for _, e := range entries {
		if e.Kind == "user" || e.Kind == "assistant" {
			res.Turns++
		}
	}
	source := "session " + m.Session
	if m.Repo != "" {
		source += " in " + m.Repo
		if m.Branch != "" {
			source += " on branch " + m.Branch
		}
	}
	res.entries, res.savedAt, res.source = entries, m.SavedAt.Format(time.RFC3339), source
	res.Rendered = res.RenderWithin(o.Budget)

	if o.Native {
		id, err := newSessionID()
		if err == nil {
			res.NativePath, err = transcript.WriteNative(a.Transcript, transcript.Native{
				Home: o.Home, Cwd: absDir(o.Dir), SessionID: id,
				SideDir: m.SessionDir, Files: a.Files,
			})
		}
		if err == nil {
			err = writeMemory(filepath.Join(filepath.Dir(res.NativePath), "memory"), a.Memory)
		}
		if err != nil {
			res.NativeErr = err.Error()
		} else {
			res.NativeSession = id
			res.Resumable = transcript.ReadResumable(a.Transcript)
		}
	}

	switch {
	case m.Base == "":
		res.Worktree = "the save has no working copy"
	default:
		res.Branch = "sesh-restore-" + o.Now.UTC().Format("20060102-150405")
		if err := worktree.Restore(o.Dir, m.Base, res.Branch, a.Patches, m.Deleted); err != nil {
			res.Branch = ""
			res.Worktree = err.Error()
		}
	}
	return res, nil
}

// Fetch downloads and opens the archive a share token names.
func Fetch(ctx context.Context, st store.Store, tok token.Token) (*archive.Archive, error) {
	blob, err := st.Get(ctx, vault.ArchiveName(tok))
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("no saved session for this token: %w", err)
	}
	if err != nil {
		return nil, err
	}
	return archive.Open(blob, tok.Key())
}

// newSessionID returns a random UUID in Claude Code's session id form.
func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func absDir(dir string) string {
	if a, err := filepath.Abs(dir); err == nil {
		return a
	}
	return dir
}

// maxMemory caps the memory folder carried in a save.
const maxMemory = 8 << 20

// readMemory reads the regular files under a Claude Code project memory
// folder, keyed by slash-separated relative path. A missing folder is empty.
func readMemory(dir string) (map[string][]byte, error) {
	files := map[string][]byte{}
	total := 0
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) && p == dir {
				return filepath.SkipDir
			}
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if total += len(data); total > maxMemory {
			return fmt.Errorf("memory folder is larger than %d bytes", maxMemory)
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = data
		return nil
	})
	return files, err
}

// maxSessionFiles caps the session files kept in one save.
const maxSessionFiles = 64 << 20

// readSessionFiles reads the parts of a session's folder that belong to its
// history (transcript.SideDirs), smallest first, leaving out files past
// maxSessionFiles. It returns how many were left out.
func readSessionFiles(dir string) (map[string][]byte, int, error) {
	type file struct {
		rel  string
		size int64
	}
	var found []file
	for _, sub := range transcript.SideDirs {
		root := filepath.Join(dir, sub)
		err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, os.ErrNotExist) && p == root {
					return filepath.SkipDir
				}
				return err
			}
			if !d.Type().IsRegular() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return err
			}
			found = append(found, file{filepath.ToSlash(rel), info.Size()})
			return nil
		})
		if err != nil {
			return nil, 0, err
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].size < found[j].size })
	files := map[string][]byte{}
	var total int64
	skipped := 0
	for _, f := range found {
		if total+f.size > maxSessionFiles {
			skipped++
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f.rel)))
		if errors.Is(err, os.ErrNotExist) {
			continue // removed since the walk
		}
		if err != nil {
			return nil, 0, err
		}
		total += int64(len(data))
		files[f.rel] = data
	}
	return files, skipped, nil
}

// writeMemory adds memory files to a project memory folder. Files already
// there are kept, never overwritten.
func writeMemory(dir string, files map[string][]byte) error {
	for rel, data := range files {
		clean := filepath.Clean(filepath.FromSlash(rel))
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe memory path %q", rel)
		}
		p := filepath.Join(dir, clean)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		if _, err := f.Write(data); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	return nil
}
