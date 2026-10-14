package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bfulton/sesh/internal/library"
	"github.com/bfulton/sesh/internal/sesh"
	"github.com/bfulton/sesh/internal/store"
	"github.com/bfulton/sesh/internal/token"
	"github.com/bfulton/sesh/internal/vault"
)

func TestMCP(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SESH_HOME", home)
	key, _ := token.New()
	t.Setenv("SESH_KEY", key.String())
	t.Setenv("SESH_STORE", "file://"+filepath.Join(home, "store"))
	tp := filepath.Join(home, "s.jsonl")
	os.WriteFile(tp, []byte(`{"type":"ai-title","aiTitle":"Token design"}
{"type":"user","message":{"role":"user","content":"Which alphabet for the share token?"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Crockford base32, so a double-tap selects it."}]}}
`), 0o600)
	if _, err := sesh.Save(context.Background(), sesh.SaveOptions{
		Store: &store.Dir{Root: filepath.Join(home, "store")}, SessionID: "abcdef12-0000", TranscriptPath: tp,
		SkipWorktree: true, Vault: &vault.Vault{Key: key},
	}); err != nil {
		t.Fatal(err)
	}

	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search_messages","arguments":{"query":"alphabet token"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"read_messages","arguments":{"session":"abcdef","start":0}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"get_session","arguments":{"session":"no-such"}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"bogus"}`,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"list_messages","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"list_messages","arguments":{"role":"all","offset":1,"limit":1,"max_chars":9}}}`,
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"list_messages","arguments":{"since":"2099-01-01"}}}`,
		`{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"list_messages","arguments":{"role":"bogus"}}}`,
		`{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"list_messages","arguments":{"until":"31/01/2026"}}}`,
	}, "\n")
	var out strings.Builder
	if err := (&mcpServer{}).serve(strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	var resps []map[string]any
	sc := bufio.NewScanner(strings.NewReader(out.String()))
	for sc.Scan() {
		var r map[string]any
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("bad response line %q: %v", sc.Text(), err)
		}
		resps = append(resps, r)
	}
	if len(resps) != 11 {
		t.Fatalf("got %d responses, want 11 (the notification gets none):\n%s", len(resps), out.String())
	}
	text := func(r map[string]any) string {
		res, _ := r["result"].(map[string]any)
		content, _ := res["content"].([]any)
		if len(content) == 0 {
			return ""
		}
		return content[0].(map[string]any)["text"].(string)
	}
	if v := resps[0]["result"].(map[string]any)["protocolVersion"]; v != "2025-06-18" {
		t.Errorf("protocol version = %v", v)
	}
	if tools := resps[1]["result"].(map[string]any)["tools"].([]any); len(tools) != 6 {
		t.Errorf("%d tools listed", len(tools))
	}
	if s := text(resps[2]); !strings.Contains(s, "message #0") || !strings.Contains(s, "Token design") {
		t.Errorf("search_messages: %s", s)
	}
	if s := text(resps[3]); !strings.Contains(s, "#1 assistant:\nCrockford base32") {
		t.Errorf("read_messages: %s", s)
	}
	if r := resps[4]["result"].(map[string]any); r["isError"] != true {
		t.Errorf("unknown session should be a tool error: %v", r)
	}
	if resps[5]["error"] == nil {
		t.Error("unknown method should be a JSON-RPC error")
	}
	if s := text(resps[6]); !strings.HasPrefix(s, "1 user messages in 1 sessions; these are 0–0") ||
		!strings.Contains(s, "message #0, user:\nWhich alphabet for the share token?") || strings.Contains(s, "more;") {
		t.Errorf("list_messages: %s", s)
	}
	if s := text(resps[7]); !strings.HasPrefix(s, "2 messages in 1 sessions; these are 1–1") ||
		!strings.Contains(s, "message #1, assistant:\nCrockford… [36 more bytes]") || strings.Contains(s, "more;") {
		t.Errorf("list_messages, every kind, paged and cut: %s", s)
	}
	if s := text(resps[8]); !strings.HasPrefix(s, "0 user messages in 0 sessions; none") {
		t.Errorf("list_messages after the last session: %s", s)
	}
	for _, r := range resps[9:] {
		if r := r["result"].(map[string]any); r["isError"] != true {
			t.Errorf("bad list_messages arguments should be a tool error: %v", r)
		}
	}
}

// A session a restore adds to the cache is visible at once, not only after
// the next pull a minute later.
func TestMCPSeesRestoredSession(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("SESH_HOME", home)
	key, _ := token.New()
	t.Setenv("SESH_KEY", key.String())
	t.Setenv("SESH_STORE", "file://"+filepath.Join(home, "store"))
	st := &store.Dir{Root: filepath.Join(home, "store")}
	tp := filepath.Join(home, "theirs.jsonl")
	os.WriteFile(tp, []byte(`{"type":"user","message":{"role":"user","content":"Someone else's session."}}
`), 0o600)
	saved, err := sesh.Save(ctx, sesh.SaveOptions{Store: st, SessionID: "theirs", TranscriptPath: tp, SkipWorktree: true, TokenDir: filepath.Join(home, "their-tokens")})
	if err != nil {
		t.Fatal(err)
	}

	srv := &mcpServer{}
	if lib, err := srv.library(ctx); err != nil || len(lib.Entries) != 0 {
		t.Fatalf("library before restore: %v, %d entries", err, len(lib.Entries))
	}
	a, err := sesh.Fetch(ctx, st, saved.Token)
	if err != nil {
		t.Fatal(err)
	}
	if err := library.Remember(cacheDir(), &vault.Vault{Key: key}, saved.Token, a); err != nil {
		t.Fatal(err)
	}
	if lib, err := srv.library(ctx); err != nil || len(lib.Find("theirs")) != 1 {
		t.Fatalf("restored session not visible: %v", err)
	}
}

// Sessions restored one right after another are each visible at once,
// however close a restore comes to the server's last read.
func TestMCPSeesEveryRestore(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("SESH_HOME", home)
	key, _ := token.New()
	t.Setenv("SESH_KEY", key.String())
	t.Setenv("SESH_STORE", "file://"+filepath.Join(home, "store"))
	st := &store.Dir{Root: filepath.Join(home, "store")}
	srv := &mcpServer{}
	if _, err := srv.library(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		id := fmt.Sprintf("restored-%d", i)
		tp := filepath.Join(home, id+".jsonl")
		os.WriteFile(tp, []byte(`{"type":"user","message":{"role":"user","content":"Session `+id+`."}}`+"\n"), 0o600)
		saved, err := sesh.Save(ctx, sesh.SaveOptions{Store: st, SessionID: id, TranscriptPath: tp, SkipWorktree: true, TokenDir: filepath.Join(home, "tokens")})
		if err != nil {
			t.Fatal(err)
		}
		a, err := sesh.Fetch(ctx, st, saved.Token)
		if err != nil {
			t.Fatal(err)
		}
		if err := library.Remember(cacheDir(), &vault.Vault{Key: key}, saved.Token, a); err != nil {
			t.Fatal(err)
		}
		if lib, err := srv.library(ctx); err != nil || len(lib.Find(id)) != 1 {
			t.Fatalf("restore %d not visible: %v", i, err)
		}
	}
}
