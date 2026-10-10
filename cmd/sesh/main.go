// Command sesh saves an agent session to your own bucket and restores it in
// another session, account or machine.
//
//	sesh save                 save this session; print its token
//	sesh restore <token>      restore a saved session here
//	sesh hook                 run as a Claude Code UserPromptSubmit hook
//
// The store is set with SESH_STORE: s3://bucket/prefix or file:///path.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bfulton/sesh/internal/sesh"
	"github.com/bfulton/sesh/internal/store"
	"github.com/bfulton/sesh/internal/token"
	"github.com/bfulton/sesh/internal/transcript"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "save":
		err = cmdSave(os.Args[2:])
	case "restore":
		err = cmdRestore(os.Args[2:])
	case "hook":
		err = cmdHook(os.Stdin, os.Stdout)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "sesh:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  sesh save [-transcript path] [-session id] [-dir dir]
  sesh restore [-dir dir] [-budget bytes] [-o file] <token>
  sesh hook    (Claude Code UserPromptSubmit hook; reads the hook event on stdin)

SESH_STORE selects the store: s3://bucket/prefix or file:///path.`)
	os.Exit(2)
}

func openStore() (store.Store, error) {
	u := os.Getenv("SESH_STORE")
	if u == "" {
		return nil, errors.New("SESH_STORE is not set; set it to s3://bucket/prefix or file:///path")
	}
	return store.Open(u)
}

func tokenDir() string {
	if d := os.Getenv("SESH_TOKEN_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".sesh", "tokens")
}

func budget() int {
	if n, err := strconv.Atoi(os.Getenv("SESH_BUDGET")); err == nil && n > 0 {
		return n
	}
	return sesh.DefaultBudget
}

func cmdSave(args []string) error {
	fs := flag.NewFlagSet("save", flag.ExitOnError)
	tpath := fs.String("transcript", "", "transcript file (default: this Claude Code session's)")
	sid := fs.String("session", os.Getenv("CLAUDE_CODE_SESSION_ID"), "session id")
	dir := fs.String("dir", ".", "working directory to capture")
	fs.Parse(args)
	if *tpath == "" {
		if *sid == "" {
			return errors.New("no session: pass -transcript, or run inside Claude Code")
		}
		home, _ := os.UserHomeDir()
		p, err := transcript.Find(home, *sid)
		if err != nil {
			return err
		}
		*tpath = p
	}
	if *sid == "" {
		*sid = strings.TrimSuffix(filepath.Base(*tpath), ".jsonl")
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	res, err := sesh.Save(context.Background(), sesh.SaveOptions{
		Store: st, SessionID: *sid, TranscriptPath: *tpath, Dir: *dir, TokenDir: tokenDir(),
	})
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, saveSummary(res))
	fmt.Println(res.Token)
	return nil
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
	tok, err := token.Parse(fs.Arg(0))
	if err != nil {
		return err
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	// From a shell there is no live session to load history into, so the
	// history becomes a native session that claude --resume continues.
	res, err := sesh.Restore(context.Background(), sesh.RestoreOptions{
		Store: st, Token: tok, Dir: *dir, Budget: *size, Native: true, Home: home,
	})
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, restoreSummary(res))
	if *out != "" {
		if err := os.WriteFile(*out, []byte(res.Rendered), 0o600); err != nil {
			return err
		}
	}
	if res.NativeSession == "" {
		return fmt.Errorf("could not write a native session: %s", res.NativeErr)
	}
	fmt.Printf("claude --resume %s\n", res.NativeSession)
	return nil
}

// hookInput is the subset of a UserPromptSubmit event sesh uses.
type hookInput struct {
	Prompt         string `json:"prompt"`
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
}

// hookOutput is a UserPromptSubmit hook response. Save uses a block, whose
// reason is shown to the user and kept out of the model's context. Restore
// stops the turn with the history as stopReason, which the model sees on
// later turns. systemMessage carries a short note for the user either way.
type hookOutput struct {
	Decision      string `json:"decision,omitempty"`
	Reason        string `json:"reason,omitempty"`
	Continue      *bool  `json:"continue,omitempty"`
	StopReason    string `json:"stopReason,omitempty"`
	SystemMessage string `json:"systemMessage,omitempty"`
}

var (
	saveRe    = regexp.MustCompile(`(?i)^\s*sesh\s+save\s*$`)
	restoreRe = regexp.MustCompile(`(?i)^\s*sesh\s+restore(?:\s+(\S+))?\s*$`)
)

// cmdHook handles "sesh save" and "sesh restore <token>" typed as prompts.
// Any other prompt passes through untouched. Neither command reaches the model.
func cmdHook(in io.Reader, out io.Writer) error {
	var ev hookInput
	if err := json.NewDecoder(in).Decode(&ev); err != nil {
		return fmt.Errorf("reading hook event: %w", err)
	}
	var resp hookOutput
	switch {
	case saveRe.MatchString(ev.Prompt):
		resp = hookSave(ev)
	case restoreRe.MatchString(ev.Prompt):
		resp = hookRestore(ev, restoreRe.FindStringSubmatch(ev.Prompt)[1])
	default:
		return nil
	}
	return json.NewEncoder(out).Encode(resp)
}

func block(msg string) hookOutput {
	return hookOutput{Decision: "block", Reason: msg, SystemMessage: msg}
}

func hookSave(ev hookInput) hookOutput {
	st, err := openStore()
	if err != nil {
		return block("sesh save failed: " + err.Error())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	res, err := sesh.Save(ctx, sesh.SaveOptions{
		Store: st, SessionID: ev.SessionID, TranscriptPath: ev.TranscriptPath, Dir: ev.Cwd, TokenDir: tokenDir(),
	})
	if err != nil {
		return block("sesh save failed: " + err.Error())
	}
	return block(saveSummary(res) + "\n\nToken:\n" + res.Token.String() + "\n\nIn another session, send:\nsesh restore " + res.Token.String())
}

func hookRestore(ev hookInput, arg string) hookOutput {
	if arg == "" {
		return block("usage: sesh restore <token>")
	}
	tok, err := token.Parse(arg)
	if err != nil {
		return block("sesh restore failed: " + err.Error())
	}
	st, err := openStore()
	if err != nil {
		return block("sesh restore failed: " + err.Error())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	// A hosted session's transcript lives on the provider's server, so a
	// native session file written there would never be read. A local CLI
	// can resume one, so write it there too.
	home, _ := os.UserHomeDir()
	res, err := sesh.Restore(ctx, sesh.RestoreOptions{
		Store: st, Token: tok, Dir: ev.Cwd, Budget: budget(),
		Native: os.Getenv("CLAUDE_CODE_REMOTE") != "true", Home: home,
	})
	if err != nil {
		return block("sesh restore failed: " + err.Error())
	}
	f := false
	return hookOutput{Continue: &f, StopReason: res.Rendered, SystemMessage: restoreSummary(res)}
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
	}
	return s
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
