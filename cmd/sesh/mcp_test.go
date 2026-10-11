package main

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	if len(resps) != 6 {
		t.Fatalf("got %d responses, want 6 (the notification gets none):\n%s", len(resps), out.String())
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
	if tools := resps[1]["result"].(map[string]any)["tools"].([]any); len(tools) != 5 {
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
}
