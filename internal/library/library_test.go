package library

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bfulton/sesh/internal/sesh"
	"github.com/bfulton/sesh/internal/store"
	"github.com/bfulton/sesh/internal/token"
	"github.com/bfulton/sesh/internal/vault"
)

func TestTerms(t *testing.T) {
	got := Terms("SessionToken parse_tokens, Queries & the UUIDs: café x 42")
	want := []string{"session", "token", "parse", "token", "query", "the", "uuid", "café", "42"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Terms = %q, want %q", got, want)
	}
	for _, keep := range []string{"class", "status", "analysis"} {
		if got := Terms(keep); len(got) != 1 || got[0] != keep {
			t.Errorf("Terms(%q) = %q", keep, got)
		}
	}
}

func TestIndexRanking(t *testing.T) {
	docs := []Doc{
		{Session: "a", Kind: "assistant", Text: "We chose Crockford base32 for the share token so it selects as one word."},
		{Session: "b", Kind: "tool_result", Text: strings.Repeat("token token token noise ", 50)},
		{Session: "c", Kind: "user", Text: "How do I deploy the server?"},
		{Session: "d", Kind: "user", Text: "base32 is fine"},
	}
	ix := NewIndex(docs)
	m := ix.Search("share token base32")
	if len(m) == 0 || docs[m[0].Doc].Session != "a" {
		t.Fatalf("top match = %+v, want session a", m)
	}
	for _, x := range m {
		if docs[x.Doc].Session == "b" || docs[x.Doc].Session == "d" {
			t.Errorf("doc without every term ranked when one has them all: %s", docs[x.Doc].Session)
		}
	}
	// No doc has every term: fall back to the docs with the most.
	if m := ix.Search("deploy kubernetes"); len(m) != 1 || docs[m[0].Doc].Session != "c" {
		t.Errorf("partial match = %+v", m)
	}
	if m := ix.Search("zzz"); len(m) != 0 {
		t.Errorf("nonsense matched: %+v", m)
	}
}

func TestSnippet(t *testing.T) {
	text := strings.Repeat("alpha ", 100) + "the needle is here " + strings.Repeat("omega ", 100)
	s := Snippet(text, "needle", 80)
	if !strings.Contains(s, "needle") || !strings.HasPrefix(s, "…") || !strings.HasSuffix(s, "…") || len(s) > 90 {
		t.Errorf("Snippet = %q", s)
	}
}

func TestLibraryPullAndSearch(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	st := &store.Dir{Root: filepath.Join(tmp, "store")}
	key, _ := token.New()
	v := &vault.Vault{Key: key}
	save := func(id, title, body string) {
		p := filepath.Join(tmp, id+".jsonl")
		os.WriteFile(p, []byte(`{"type":"custom-title","customTitle":"`+title+`"}
{"type":"user","message":{"role":"user","content":"`+body+`"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Done."},{"type":"tool_use","id":"t","name":"Bash","input":{"command":"go test ./..."}}]}}
`), 0o600)
		if _, err := sesh.Save(ctx, sesh.SaveOptions{Store: st, SessionID: id, TranscriptPath: p, SkipWorktree: true, Vault: v}); err != nil {
			t.Fatal(err)
		}
	}
	save("s-one", "Token format", "Make the share token easy to select on a phone.")
	save("s-two", "Deploy", "Deploy the blob server to Fly.")

	cache := filepath.Join(tmp, "cache")
	lib, err := Open(ctx, st, v, cache, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(lib.Entries) != 2 {
		t.Fatalf("entries = %d", len(lib.Entries))
	}
	hits := lib.Search("phone token", 5)
	if len(hits) != 1 || hits[0].Entry.Session != "s-one" || !strings.Contains(hits[0].Snippet, "phone") {
		t.Fatalf("hits = %+v", hits)
	}
	if hits := lib.Search("deploy", 5); len(hits) != 1 || hits[0].Entry.Title != "Deploy" {
		t.Fatalf("title/body search = %+v", hits)
	}

	// Offline, the cache alone answers.
	off, err := Open(ctx, nil, v, cache, false)
	if err != nil || len(off.Search("phone", 5)) != 1 {
		t.Fatalf("offline open: %v", err)
	}
	two := off.Find("s-two")
	if len(two) != 1 {
		t.Fatalf("Find(s-two) = %+v", two)
	}
	r, err := off.Render(two[0].ID(), 10000)
	if err != nil || !strings.Contains(r, "Fly") {
		t.Fatalf("render: %v %q", err, r)
	}

	// A newer save is pulled; an unchanged one is not re-downloaded.
	time.Sleep(10 * time.Millisecond)
	save("s-two", "Deploy", "Deploy the blob server to Fly and Railway.")
	lib2, err := Open(ctx, st, v, cache, true)
	if err != nil {
		t.Fatal(err)
	}
	if h := lib2.Search("railway", 5); len(h) != 1 {
		t.Fatalf("updated session not pulled: %+v", h)
	}
}

// A cache written while sesh keyed sessions by id and folder can hold two
// files for one session; loading keeps the latest save and removes the rest.
func TestLoadCacheOnePerSession(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	for i, name := range []string{"old-folder", "new-folder", "newest"} {
		c := cached{Format: cacheFormat, Entry: vault.Entry{Session: "s1", Title: name, SavedAt: now.Add(time.Duration(i) * time.Minute)}}
		data, _ := json.Marshal(c)
		os.WriteFile(filepath.Join(dir, name+".json"), data, 0o600)
	}
	got, err := loadCache(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["s1"].Entry.Title != "newest" {
		t.Fatalf("loadCache = %+v", got)
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "*.json")); len(files) != 1 {
		t.Errorf("cache files left: %v", files)
	}
}
