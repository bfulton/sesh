package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bfulton/sesh/internal/redact"
	"github.com/bfulton/sesh/internal/sesh"
	"github.com/bfulton/sesh/internal/transcript"
)

// hookInput is the subset of a Claude Code hook event sesh uses.
type hookInput struct {
	Event          string `json:"hook_event_name"`
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	CommandName    string `json:"command_name"` // UserPromptExpansion: the slash command
	CommandArgs    string `json:"command_args"` // and what was typed after it
}

// cmdHook is the entry point for the plugin's Claude Code hooks:
//
//	Stop                 saves the session after each turn, detached so the
//	                     turn ends at once, and hands Claude a restored
//	                     history waiting for it
//	UserPromptExpansion  runs a /sesh command and hands Claude its output
//
// Every run records what it did in ~/.sesh/hook-status.json.
func cmdHook(in io.Reader, out io.Writer) error {
	var ev hookInput
	if e := os.Getenv("SESH_HOOK_EVENT"); e != "" {
		in = strings.NewReader(e)
	}
	if err := json.NewDecoder(in).Decode(&ev); err != nil {
		return fmt.Errorf("reading hook event: %w", err)
	}
	switch ev.Event {
	case "Stop", "SessionEnd":
		if history, ok := takePending(ev.SessionID); ok && ev.Event == "Stop" {
			if err := json.NewEncoder(out).Encode(map[string]any{"continue": false, "stopReason": history}); err != nil {
				return err
			}
		}
	case "UserPromptExpansion":
		if ev.CommandName != skillName {
			return nil
		}
		cmd, output := hookSkill(ev)
		recordHook(ev, "/sesh "+cmd)
		shown := "sesh printed this for the user's /sesh command:\n\n" + output
		if utf8.RuneCountInString(shown) > hookContextLimit {
			shown = transcript.Trim(shown, hookContextLimit-100)
		}
		return json.NewEncoder(out).Encode(map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName":     "UserPromptExpansion",
			"additionalContext": shown,
		}})
	default:
		return nil
	}
	// Saving can take a few seconds, so it runs detached: the turn ends at
	// once, and the save finishes even if Claude Code exits first. Saves are
	// serialized, so the last one written is the newest.
	if os.Getenv("SESH_HOOK_DETACHED") == "" {
		data, _ := json.Marshal(ev)
		if err := detach([]string{"hook"}, []string{"SESH_HOOK_DETACHED=1", "SESH_HOOK_EVENT=" + string(data)}); err == nil {
			return nil
		}
	}
	os.MkdirAll(seshHome(), 0o700)
	if unlock, err := lockFile(filepath.Join(seshHome(), "hook.lock")); err == nil {
		defer unlock()
	}
	recordHook(ev, hookKeep(ev))
	return nil
}

// hookContextLimit is the most hook context, in characters, that Claude Code
// hands the model as is. Past about 10,000 the model gets only a short
// preview and a file path: with Claude Code 2.1.296, 9,516 characters
// arrived whole and 10,516 didn't. skillHistory is how much of a restored
// session's history fits beside sesh's summary.
const (
	hookContextLimit = 9_500
	skillHistory     = 8_000
)

// skillName is the /sesh skill's command name: plugin skills are named
// plugin:skill, however they are typed.
const skillName = "sesh:sesh"

// skillScanBudget limits the time /sesh save spends scanning for secrets,
// within the 180 s Claude Code gives its hook.
const skillScanBudget = 2 * time.Minute

// hookSkill runs the /sesh command in ev and returns its name and output,
// errors included. The command arrives as the hook's command_args, data
// rather than a shell command line, so nothing typed or pasted after /sesh
// can run as a command; and the words after the command are its argument,
// never flags.
func hookSkill(ev hookInput) (cmd, output string) {
	cmd, rest := "status", ""
	if f := strings.Fields(ev.CommandArgs); len(f) > 0 {
		cmd, rest = f[0], strings.Join(f[1:], " ")
	}
	var b strings.Builder
	var err error
	switch cmd {
	case "status":
		b.WriteString(statusText())
	case "list":
		err = listSessions(&b, 20, false)
	case "search":
		if rest == "" {
			err = errors.New("usage: /sesh search WORDS")
		} else {
			err = searchSessions(&b, rest, 10, false)
		}
	case "save":
		if _, statErr := os.Stat(ev.TranscriptPath); statErr != nil {
			err = errors.New("nothing to save yet: this session has no finished turns")
		} else {
			err = saveSession(&b, &b, ev.SessionID, ev.TranscriptPath, ev.Cwd, skillScanBudget)
		}
	case "restore":
		if rest == "" {
			err = errors.New("usage: /sesh restore TARGET, a share token, session id or title words")
		} else if inCloud() {
			err = hookRestore(&b, ev, rest)
		} else {
			err = restoreSession(&b, &b, rest, ev.Cwd, budget(), "")
		}
	default:
		err = fmt.Errorf("/sesh runs status, list, search WORDS, save and restore TARGET; %q isn't one of them", cmd)
	}
	if err != nil {
		fmt.Fprintln(&b, "sesh:", err)
	}
	return cmd, b.String()
}

// hookRestore restores a session into a cloud session in two steps. Hook
// context reaches the model whole only up to hookContextLimit, while a
// Stop hook's stopReason does at any size but shows nothing in the Claude
// app. So the restore turn gets the end of the history, for the model to
// say where the session stopped, and the Stop hook that ends the turn
// appends the whole history, for every turn after.
func hookRestore(w io.Writer, ev hookInput, target string) error {
	res, readable, err := restoreRun(target, ev.Cwd, budget())
	if err != nil {
		return err
	}
	full := res.Rendered
	if readable {
		full += libraryNote(res)
	}
	if err := putPending(ev.SessionID, full); err != nil {
		return err
	}
	fmt.Fprintln(w, restoreSummary(res))
	fmt.Fprint(w, res.RenderWithin(skillHistory))
	fmt.Fprintf(w, "\n[sesh] That is the end of the history. When this turn ends, the whole rendered history, %d KB, is added to your context.\n", (len(full)+999)/1000)
	return nil
}

// A restored history waits in ~/.sesh/pending/<session id> for the Stop
// hook that ends the restore turn. Waiting files older than a day belong to
// sessions that ended first, and are removed.
func pendingPath(sessionID string) (string, bool) {
	if sessionID == "" || strings.ContainsAny(sessionID, `/\.`) {
		return "", false
	}
	return filepath.Join(seshHome(), "pending", sessionID), true
}

func putPending(sessionID, history string) error {
	p, ok := pendingPath(sessionID)
	if !ok {
		return fmt.Errorf("no usable session id %q to restore into", sessionID)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	if old, err := os.ReadDir(filepath.Dir(p)); err == nil {
		for _, e := range old {
			if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > 24*time.Hour {
				os.Remove(filepath.Join(filepath.Dir(p), e.Name()))
			}
		}
	}
	return os.WriteFile(p, []byte(history), 0o600)
}

func takePending(sessionID string) (string, bool) {
	p, ok := pendingPath(sessionID)
	if !ok {
		return "", false
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", false
	}
	os.Remove(p)
	return string(data), true
}

// hookScanBudget limits the time a hook's save spends scanning for secrets,
// within its five minutes. Scanning a long session for the first time can
// take longer; the scan cache keeps what was scanned, so the next turn's save
// carries on, instead of starting over when the context runs out.
const hookScanBudget = 3 * time.Minute

// hookKeep saves the session after a turn, if a vault and store are set up.
func hookKeep(ev hookInput) string {
	v, err := loadVault()
	if err != nil {
		return "error: " + err.Error()
	}
	if v == nil || storeURL() == "" {
		return "not configured (no vault key or store); nothing saved"
	}
	st, err := openStore()
	if err != nil {
		return "error: " + err.Error()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	res, err := sesh.Save(ctx, sesh.SaveOptions{
		Store: st, SessionID: ev.SessionID, TranscriptPath: ev.TranscriptPath, Dir: ev.Cwd,
		Vault: v, Host: hostLabel(),
		KeepSecrets: keepSecrets(), ScanCache: scanCache(ev.SessionID), ScanBudget: hookScanBudget,
	})
	var inc *redact.Incomplete
	if errors.As(err, &inc) {
		return inc.Error()
	}
	if err != nil {
		return "error: " + err.Error()
	}
	markSynced(ev.TranscriptPath)
	return saveSummary(res)
}

func statusText() string {
	var b strings.Builder
	b.WriteString("sesh status\n")
	v, err := loadVault()
	switch {
	case err != nil:
		fmt.Fprintf(&b, "vault: error: %v\n", err)
	case v == nil:
		b.WriteString("vault: none (run sesh init, or set SESH_KEY)\n")
	default:
		fmt.Fprintf(&b, "vault: %s…\n", v.ID()[:8])
	}
	if u := storeURL(); u != "" {
		fmt.Fprintf(&b, "store: %s\n", redactURL(u))
	} else {
		b.WriteString("store: none (run sesh init, or set SESH_STORE)\n")
	}
	if keepSecrets() {
		b.WriteString("redaction: off (redact=off)\n")
	} else {
		b.WriteString("redaction: on\n")
	}
	if data, err := os.ReadFile(filepath.Join(seshHome(), "hook-status.json")); err == nil {
		fmt.Fprintf(&b, "last hook run: %s\n", strings.TrimSpace(string(data)))
	}
	return b.String()
}

func redactURL(u string) string {
	if i := strings.Index(u, "@"); i >= 0 && strings.Contains(u[:i], "://") {
		return u[:strings.Index(u, "://")+3] + "…" + u[i:]
	}
	return u
}

// recordHook writes the latest hook run to ~/.sesh/hook-status.json, so a
// person can check that hooks run at all, e.g. in a cloud session.
func recordHook(ev hookInput, result string) {
	rec := map[string]string{
		"time":    time.Now().UTC().Format(time.RFC3339),
		"event":   ev.Event,
		"session": ev.SessionID,
		"host":    hostLabel(),
		"result":  result,
	}
	data, _ := json.Marshal(rec)
	os.MkdirAll(seshHome(), 0o700)
	os.WriteFile(filepath.Join(seshHome(), "hook-status.json"), append(data, '\n'), 0o600)
}
