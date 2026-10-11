package vault

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bfulton/sesh/internal/store"
	"github.com/bfulton/sesh/internal/token"
)

func newVault(t *testing.T) Vault {
	k, err := token.New()
	if err != nil {
		t.Fatal(err)
	}
	return Vault{Key: k}
}

func TestDerivations(t *testing.T) {
	a, b := newVault(t), newVault(t)
	if a.SessionToken("s1") != a.SessionToken("s1") {
		t.Error("session token is not deterministic")
	}
	if a.SessionToken("s1") == a.SessionToken("s2") || a.SessionToken("s1") == b.SessionToken("s1") {
		t.Error("session tokens collide across sessions or vaults")
	}
	if a.SessionToken("s1") == a.Key {
		t.Error("session token equals the vault key")
	}
	if a.ID() == b.ID() || len(a.ID()) != 32 {
		t.Errorf("vault ids: %q %q", a.ID(), b.ID())
	}
	for _, n := range []string{a.EntryName("s1"), ArchiveName(a.SessionToken("s1"))} {
		if err := store.CheckName(n); err != nil {
			t.Error(err)
		}
	}
}

func TestCatalog(t *testing.T) {
	ctx := context.Background()
	st := &store.Dir{Root: t.TempDir()}
	v, other := newVault(t), newVault(t)
	now := time.Now().UTC().Truncate(time.Second)
	for i, id := range []string{"aaa-1", "bbb-2", "ccc-3"} {
		e := Entry{Session: id, Source: "claude-code", Title: "Fix the " + id, SavedAt: now.Add(time.Duration(i) * time.Minute), Messages: i}
		if err := v.Put(ctx, st, e); err != nil {
			t.Fatal(err)
		}
	}
	if err := other.Put(ctx, st, Entry{Session: "zzz", SavedAt: now}); err != nil {
		t.Fatal(err)
	}

	entries, bad, err := v.List(ctx, st)
	if err != nil || bad != 0 {
		t.Fatalf("List: %v, bad=%d", err, bad)
	}
	if len(entries) != 3 || entries[0].Session != "ccc-3" || entries[2].Session != "aaa-1" {
		t.Fatalf("List = %+v, want 3 entries newest first, other vault excluded", entries)
	}
	got, err := v.Get(ctx, st, "bbb-2")
	if err != nil || got.Title != "Fix the bbb-2" {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if f := Find(entries, "bb"); len(f) != 1 || f[0].Session != "bbb-2" {
		t.Errorf("Find by id prefix = %+v", f)
	}
	if f := Find(entries, "FIX THE C"); len(f) != 1 || f[0].Session != "ccc-3" {
		t.Errorf("Find by title = %+v", f)
	}

	// An entry copied to another session's name doesn't decrypt there.
	sealed, _ := st.Get(ctx, v.EntryName("aaa-1"))
	st.Put(ctx, v.EntryName("bbb-2"), sealed)
	if _, err := v.Get(ctx, st, "bbb-2"); !errors.Is(err, errDecrypt) {
		t.Errorf("swapped entry: err = %v, want errDecrypt", err)
	}
	if _, bad, _ := v.List(ctx, st); bad != 1 {
		t.Errorf("List bad = %d, want 1", bad)
	}
}
