// Package sesh implements save and restore: a session's transcript and
// working copy, sealed under a token and kept in a store.
package sesh

import (
	"context"
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
	TokenDir       string // where each session's token is kept between saves
	Now            time.Time
}

// SaveResult describes a completed save.
type SaveResult struct {
	Token    token.Token
	Manifest archive.Manifest
	Turns    int    // user and assistant messages in the transcript
	Worktree string // why the working copy was not captured, if it wasn't
	Pushed   bool   // whether the base commit is on a remote
}

// Save captures and uploads a session. A session keeps one token across
// saves, so its token always restores the latest save.
func Save(ctx context.Context, o SaveOptions) (*SaveResult, error) {
	data, err := os.ReadFile(o.TranscriptPath)
	if err != nil {
		return nil, fmt.Errorf("reading transcript: %w", err)
	}
	entries, err := transcript.Parse(data)
	if err != nil {
		return nil, err
	}
	tok, err := sessionToken(o.TokenDir, o.SessionID)
	if err != nil {
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
	res := &SaveResult{Token: tok}
	for _, e := range entries {
		if e.Kind == "user" || e.Kind == "assistant" {
			res.Turns++
		}
	}
	if c, err := worktree.Take(o.Dir); err != nil {
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
	if err := o.Store.Put(ctx, tok.ID(), blob); err != nil {
		return nil, fmt.Errorf("uploading: %w", err)
	}
	res.Manifest = a.Manifest
	for p := range a.Patches {
		res.Manifest.Patches = append(res.Manifest.Patches, p)
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
}

// RestoreResult describes a completed restore.
type RestoreResult struct {
	Manifest archive.Manifest
	Turns    int    // user and assistant messages restored
	Rendered string // the history, ready to place in the model's context
	Branch   string // branch the working copy was restored onto, if any
	Worktree string // why the working copy was not restored, if it wasn't
	Archive  *archive.Archive
}

// Restore downloads a saved session, restores its working copy into Dir and
// renders its history. A working-copy problem does not stop the history from
// being restored; it is reported in Worktree instead.
func Restore(ctx context.Context, o RestoreOptions) (*RestoreResult, error) {
	blob, err := o.Store.Get(ctx, o.Token.ID())
	if err != nil {
		return nil, err
	}
	a, err := archive.Open(blob, o.Token.Key())
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
