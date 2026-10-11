package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bfulton/sesh/internal/library"
	"github.com/bfulton/sesh/internal/sesh"
	"github.com/bfulton/sesh/internal/store"
	"github.com/bfulton/sesh/internal/token"
	"github.com/bfulton/sesh/internal/transcript"
	"github.com/bfulton/sesh/internal/vault"
)

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	storeFlag := fs.String("store", "", "storage URL: file:///path, s3://bucket/prefix or https://sesh-server")
	keyFlag := fs.String("key", "", "use this existing vault key (from another device)")
	fs.Parse(args)
	if inCloud() {
		return errors.New("a cloud session's files don't outlast it, so a vault set up here would be lost. " +
			"Run sesh init on your own device, then set SESH_KEY and SESH_STORE in the cloud environment's settings")
	}

	keyPath := filepath.Join(seshHome(), "key")
	v, err := loadVault()
	if err != nil {
		return err
	}
	switch {
	case *keyFlag != "":
		k, err := token.Parse(*keyFlag)
		if err != nil {
			return fmt.Errorf("vault key: %w", err)
		}
		if v != nil && v.Key != k {
			return fmt.Errorf("a different vault key is already in %s; remove it first", keyPath)
		}
		v = &vault.Vault{Key: k}
	case v == nil:
		k, err := token.New()
		if err != nil {
			return err
		}
		v = &vault.Vault{Key: k}
	}
	if err := os.MkdirAll(seshHome(), 0o700); err != nil {
		return err
	}
	if os.Getenv("SESH_KEY") == "" {
		if err := os.WriteFile(keyPath, []byte(v.Key.String()+"\n"), 0o600); err != nil {
			return err
		}
	}
	cfg := readConfig()
	if *storeFlag != "" {
		if _, err := store.Open(*storeFlag); err != nil {
			return err
		}
		cfg["store"] = *storeFlag
	}
	if cfg["store"] == "" {
		cfg["store"] = "file://" + filepath.Join(seshHome(), "store")
	}
	if err := writeConfig(cfg); err != nil {
		return err
	}
	if inClaude() {
		fmt.Printf(`Vault set up. Storage: %s
The vault key is in %s. It isn't printed here: this session's transcript
would keep it. To set up another device, run sesh key in your own terminal.
`, redactURL(cfg["store"]), keyPath)
		return nil
	}
	fmt.Printf(`Vault key:
%s

Keep it safe: it is the only way to read your sessions. Anyone with it can.
Storage: %s

On another device:      sesh init -key <vault key> -store <storage URL>
In Claude Code cloud:   set SESH_KEY and SESH_STORE in your environment's variables
`, v.Key, cfg["store"])
	return nil
}

func cmdKey([]string) error {
	if inClaude() {
		return errors.New("not printing the vault key inside a Claude Code session: its transcript would keep it. Run sesh key in your own terminal")
	}
	v, err := requireVault()
	if err != nil {
		return err
	}
	fmt.Println(v.Key)
	return nil
}

func cmdStatus([]string) error {
	fmt.Print(statusText())
	return nil
}

func cmdSave(args []string) error {
	fs := flag.NewFlagSet("save", flag.ExitOnError)
	tpath := fs.String("transcript", "", "transcript file (default: this Claude Code session's)")
	sid := fs.String("session", "", "session id (default: the transcript's, or this Claude Code session's)")
	dir := fs.String("dir", ".", "working directory to capture")
	fs.Parse(args)
	if *tpath == "" {
		if *sid == "" {
			*sid = os.Getenv("CLAUDE_CODE_SESSION_ID")
		}
		if *sid == "" {
			return errors.New("no session: pass -transcript, or run inside Claude Code")
		}
		p, err := transcript.Find(claudeHome(), *sid)
		if err != nil {
			return err
		}
		*tpath = p
	}
	if *sid == "" {
		*sid = strings.TrimSuffix(filepath.Base(*tpath), ".jsonl")
	}
	return saveSession(os.Stdout, os.Stderr, *sid, *tpath, *dir)
}

// saveSession saves a session, writing a summary to ew and its share token
// to w.
func saveSession(w, ew io.Writer, sid, tpath, dir string) error {
	st, err := openStore()
	if err != nil {
		return err
	}
	v, err := loadVault()
	if err != nil {
		return err
	}
	res, err := sesh.Save(context.Background(), sesh.SaveOptions{
		Store: st, SessionID: sid, TranscriptPath: tpath, Dir: dir,
		Vault: v, TokenDir: tokenDir(), Host: hostLabel(),
	})
	if err != nil {
		return err
	}
	fmt.Fprintln(ew, saveSummary(res))
	fmt.Fprintln(w, res.Token)
	return nil
}

// resolveToken turns a restore argument into a share token: a token itself,
// or a session id prefix or title words looked up in the vault's catalog.
func resolveToken(ctx context.Context, st store.Store, arg string) (token.Token, error) {
	if t, err := token.Parse(arg); err == nil {
		return t, nil
	}
	v, err := loadVault()
	if err != nil {
		return token.Token{}, err
	}
	if v == nil {
		return token.Token{}, fmt.Errorf("%q is not a share token, and there is no vault to look it up in", arg)
	}
	entries, _, err := v.List(ctx, st)
	if err != nil {
		return token.Token{}, err
	}
	found := vault.Find(entries, arg)
	switch len(found) {
	case 0:
		return token.Token{}, fmt.Errorf("no saved session matches %q", arg)
	case 1:
		return token.Parse(found[0].Token)
	default:
		var b strings.Builder
		fmt.Fprintf(&b, "%d sessions match %q:", len(found), arg)
		for i, e := range found {
			if i == 8 {
				fmt.Fprintf(&b, "\n  …")
				break
			}
			fmt.Fprintf(&b, "\n  %s  %s", vault.Handle(entries, e), e.Title)
		}
		return token.Token{}, errors.New(b.String())
	}
}

func cmdRestore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	dir := fs.String("dir", ".", "working directory to restore into")
	size := fs.Int("budget", budget(), "maximum size of the rendered history in bytes")
	out := fs.String("o", "", "also write the rendered history to this file")
	fs.Parse(args)
	if fs.NArg() != 1 {
		usage()
	}
	return restoreSession(os.Stdout, os.Stderr, fs.Arg(0), *dir, *size, *out)
}

// restoreSession restores the session target names into dir, writing a
// summary to ew and to w what continues it: in a cloud session the rendered
// history, elsewhere the claude --resume command. With out, it also writes
// the rendered history to that file.
func restoreSession(w, ew io.Writer, target, dir string, size int, out string) error {
	res, readable, err := restoreRun(target, dir, size)
	if err != nil {
		return err
	}
	fmt.Fprintln(ew, restoreSummary(res))
	if out != "" {
		if err := os.WriteFile(out, []byte(res.Rendered), 0o600); err != nil {
			return err
		}
	}
	if inCloud() {
		fmt.Fprint(w, res.Rendered)
		if readable {
			fmt.Fprint(w, libraryNote(res))
		}
		return nil
	}
	if res.NativeSession == "" {
		return fmt.Errorf("could not write a native session: %s", res.NativeErr)
	}
	fmt.Fprintf(w, "claude --resume %s\n", res.NativeSession)
	return nil
}

// restoreRun restores the session target names into dir, rendering its
// history within size bytes. It reports whether sesh's search tools can read
// the whole session.
func restoreRun(target, dir string, size int) (*sesh.RestoreResult, bool, error) {
	st, err := openStore()
	if err != nil {
		return nil, false, err
	}
	ctx := context.Background()
	tok, err := resolveToken(ctx, st, target)
	if err != nil {
		return nil, false, err
	}
	// Locally the history becomes a native session that claude --resume
	// continues with its real turns. A cloud session's transcript lives on
	// the provider's servers, where a native session file would never be
	// read, so there the history goes to the model instead.
	res, err := sesh.Restore(ctx, sesh.RestoreOptions{
		Store: st, Token: tok, Dir: dir, Budget: size, Native: !inCloud(), Home: claudeHome(),
	})
	if err != nil {
		return nil, false, err
	}
	return res, remember(tok, res), nil
}

// libraryNote tells the model where to read what a rendered history left out.
func libraryNote(res *sesh.RestoreResult) string {
	return fmt.Sprintf("\n[sesh] The whole session is in sesh's library as session %s: "+
		"search_messages and read_messages can read what was trimmed or left out above.\n", res.Manifest.Session)
}

// remember adds a restored session to this device's library, so sesh's
// search tools can read what its rendered history trimmed or left out. It
// reports whether they can.
func remember(tok token.Token, res *sesh.RestoreResult) bool {
	v, err := loadVault()
	if err != nil || v == nil {
		return false
	}
	return library.Remember(filepath.Join(seshHome(), "cache"), v, tok, res.Archive) == nil
}

func saveSummary(r *sesh.SaveResult) string {
	s := fmt.Sprintf("sesh saved %d messages", r.Turns)
	switch {
	case r.Worktree != "":
		s += "; working copy not saved (" + r.Worktree + ")"
	default:
		s += fmt.Sprintf(", %d changed and %d deleted files on %s", len(r.Manifest.Patches), len(r.Manifest.Deleted), shortID(r.Manifest.Base))
		if !r.Pushed {
			s += " (base commit not pushed; restore needs it on the target)"
		}
	}
	if r.Skipped > 0 {
		s += fmt.Sprintf("; %d large tool outputs left out", r.Skipped)
	}
	return s + "."
}

func restoreSummary(r *sesh.RestoreResult) string {
	s := fmt.Sprintf("sesh restored %d messages from session %s (saved %s), %d bytes of history",
		r.Turns, r.Manifest.Session, r.Manifest.SavedAt.Format(time.RFC3339), len(r.Rendered))
	if r.Branch != "" {
		s += fmt.Sprintf("; working copy restored on branch %s (%d changed, %d deleted files)",
			r.Branch, len(r.Manifest.Patches), len(r.Manifest.Deleted))
	} else {
		s += "; working copy not restored: " + r.Worktree
	}
	s += "."
	if r.NativeSession != "" {
		s += "\nFor the real turns, start a new session here with: claude --resume " + r.NativeSession
		if n := r.Resumable; n.Compactions > 0 {
			s += fmt.Sprintf("\nIt was compacted, so Claude Code resumes it from the last compaction: %d of its %d prompts, "+
				"with a summary of the rest. sesh show %s prints the whole conversation.", n.Loaded, n.Prompts, shortID(r.Manifest.Session))
		}
	}
	return s
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
