package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/bfulton/sesh/internal/sesh"
)

// hookInput is the subset of a Claude Code hook event sesh uses.
type hookInput struct {
	Event          string `json:"hook_event_name"`
	Prompt         string `json:"prompt"`
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
}

// hookOutput is a hook response. A block's reason is shown to the user and
// kept out of the model's context. Stopping with continue=false puts
// stopReason in the model's context for later turns. systemMessage is a
// note for the user.
type hookOutput struct {
	Decision      string `json:"decision,omitempty"`
	Reason        string `json:"reason,omitempty"`
	Continue      *bool  `json:"continue,omitempty"`
	StopReason    string `json:"stopReason,omitempty"`
	SystemMessage string `json:"systemMessage,omitempty"`
}

var (
	saveRe    = regexp.MustCompile(`(?i)^\s*sesh\s+save\s*$`)
	restoreRe = regexp.MustCompile(`(?i)^\s*sesh\s+restore(?:\s+(.+?))?\s*$`)
	searchRe  = regexp.MustCompile(`(?i)^\s*sesh\s+search\s+(.+?)\s*$`)
	statusRe  = regexp.MustCompile(`(?i)^\s*sesh(?:\s+status)?\s*$`)
)

// cmdHook is the entry point for Claude Code hooks:
//
//	UserPromptSubmit  "sesh save", "sesh restore <token|id|title>", "sesh search <query>"
//	                  and "sesh status" typed as prompts; none reaches the model
//	Stop              saves the session after each turn (run it with async: true)
//
// Every run records what it did in ~/.sesh/hook-status.json.
func cmdHook(in io.Reader, out io.Writer) error {
	var ev hookInput
	if err := json.NewDecoder(in).Decode(&ev); err != nil {
		return fmt.Errorf("reading hook event: %w", err)
	}
	if ev.Event == "Stop" || ev.Event == "SessionEnd" {
		result := hookKeep(ev)
		recordHook(ev, result)
		return nil
	}
	var resp hookOutput
	switch p := ev.Prompt; {
	case saveRe.MatchString(p):
		resp = hookSave(ev)
	case restoreRe.MatchString(p):
		resp = hookRestore(ev, restoreRe.FindStringSubmatch(p)[1])
	case searchRe.MatchString(p):
		resp = hookSearch(searchRe.FindStringSubmatch(p)[1])
	case statusRe.MatchString(p):
		resp = block(statusText())
	default:
		return nil
	}
	recordHook(ev, firstLine(resp.Reason+resp.SystemMessage))
	return json.NewEncoder(out).Encode(resp)
}

func block(msg string) hookOutput {
	return hookOutput{Decision: "block", Reason: msg, SystemMessage: msg}
}

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
	})
	if err != nil {
		return "error: " + err.Error()
	}
	markSynced(ev.TranscriptPath)
	return saveSummary(res)
}

func hookSave(ev hookInput) hookOutput {
	st, err := openStore()
	if err != nil {
		return block("sesh save failed: " + err.Error())
	}
	v, err := loadVault()
	if err != nil {
		return block("sesh save failed: " + err.Error())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	res, err := sesh.Save(ctx, sesh.SaveOptions{
		Store: st, SessionID: ev.SessionID, TranscriptPath: ev.TranscriptPath, Dir: ev.Cwd,
		Vault: v, TokenDir: tokenDir(), Host: hostLabel(),
	})
	if err != nil {
		return block("sesh save failed: " + err.Error())
	}
	markSynced(ev.TranscriptPath)
	tok := res.Token.String()
	return block(saveSummary(res) + "\n\nShare token:\n" + tok + "\n\nTo continue elsewhere, send:\nsesh restore " + tok)
}

func hookRestore(ev hookInput, arg string) hookOutput {
	if arg == "" {
		return block("usage: sesh restore <token, session id or title words>")
	}
	st, err := openStore()
	if err != nil {
		return block("sesh restore failed: " + err.Error())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	tok, err := resolveToken(ctx, st, arg)
	if err != nil {
		return block("sesh restore failed: " + err.Error())
	}
	// A hosted session's transcript lives on the provider's server, so a
	// native session file written there would never be read. A local CLI
	// can resume one, so write it there too.
	res, err := sesh.Restore(ctx, sesh.RestoreOptions{
		Store: st, Token: tok, Dir: ev.Cwd, Budget: budget(),
		Native: os.Getenv("CLAUDE_CODE_REMOTE") != "true", Home: claudeHome(),
	})
	if err != nil {
		return block("sesh restore failed: " + err.Error())
	}
	f := false
	return hookOutput{Continue: &f, StopReason: res.Rendered, SystemMessage: restoreSummary(res)}
}

// hookSearch shows search results to the user without involving the model.
func hookSearch(query string) hookOutput {
	lib, err := openLibrary(context.Background(), true)
	if err != nil {
		return block("sesh search failed: " + err.Error())
	}
	hits := lib.Search(query, 8)
	if len(hits) == 0 {
		return block(fmt.Sprintf("sesh: nothing matches %q in %d sessions.", query, len(lib.Entries)))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "sesh: %d results for %q\n", len(hits), query)
	for _, h := range hits {
		fmt.Fprintf(&b, "\n%s\n", formatHit(lib, h))
	}
	b.WriteString("\nContinue one here with: sesh restore <session id>")
	return block(b.String())
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

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s
}
