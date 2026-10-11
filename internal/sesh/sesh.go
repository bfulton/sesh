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
	"strings"
	"time"

	"github.com/bfulton/sesh/internal/archive"
	"github.com/bfulton/sesh/internal/store"
	"github.com/bfulton/sesh/internal/token"
	"github.com/bfulton/sesh/internal/transcript"
	"github.com/bfulton/sesh/internal/vault"
	"github.com/bfulton/sesh/internal/worktree"
)

// DefaultBudget is the default size of a rendered history, in bytes.
const DefaultBudget = 300_000

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
}

// SaveResult describes a completed save.
type SaveResult struct {
	Token    token.Token
	Manifest archive.Manifest
	Turns    int    // user and assistant messages in the transcript
	Worktree string // why the working copy was not captured, if it wasn't
	Pushed   bool   // whether the base commit is on a remote
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
		tok = o.Vault.SessionToken(SessionKey(o.SessionID, o.TranscriptPath))
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
		e := vault.Entry{
			Key: SessionKey(o.SessionID, o.TranscriptPath), Session: o.SessionID, Source: a.Manifest.Source, Title: meta.Title,
			Cwd: meta.Cwd, Repo: a.Manifest.Repo, Branch: meta.Branch, Started: meta.Started, Updated: meta.Updated,
			SavedAt: a.Manifest.SavedAt, Messages: meta.Messages, Bytes: len(data),
			Token: tok.String(), Host: o.Host,
		}
		if a.Manifest.Branch != "" {
			e.Branch = a.Manifest.Branch
		}
		// The catalog is written after the archive, so an entry never points
		// at an archive that isn't there.
		if err := o.Vault.Put(ctx, o.Store, e); err != nil {
			return nil, fmt.Errorf("updating catalog: %w", err)
		}
		res.Entry = &e
	}
	return res, nil
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

	NativeSession string // id of the native session written, if any
	NativePath    string // its transcript file
	NativeErr     string // why it couldn't be written, if Native was asked for
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
	res.Rendered = transcript.Render(entries, m.SavedAt.Format(time.RFC3339), source, o.Budget)

	if o.Native {
		id, err := newSessionID()
		if err == nil {
			res.NativePath, err = transcript.WriteNative(a.Transcript, o.Home, absDir(o.Dir), id)
		}
		if err == nil {
			err = writeMemory(filepath.Join(filepath.Dir(res.NativePath), "memory"), a.Memory)
		}
		if err != nil {
			res.NativeErr = err.Error()
		} else {
			res.NativeSession = id
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

// SessionKey identifies a session in a vault: its id plus the project folder
// holding its transcript. Claude Code session ids are unique, but the same
// id can appear in two project folders (a session written to two folders,
// or a nested session that inherited its parent's id), and those are
// different sessions. Every device and hook that saves a transcript derives
// the same key from it.
func SessionKey(sessionID, transcriptPath string) string {
	if transcriptPath == "" {
		return sessionID
	}
	return sessionID + "@" + filepath.Base(filepath.Dir(transcriptPath))
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
