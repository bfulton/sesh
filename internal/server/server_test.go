package server

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/bfulton/sesh/internal/store"
)

func client(t *testing.T, url, token string) *store.Remote {
	r, err := store.NewRemote(url)
	if err != nil {
		t.Fatal(err)
	}
	r.Token = token
	return r
}

func TestServer(t *testing.T) {
	srv, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	ctx := context.Background()
	alice := client(t, ts.URL, strings.Repeat("a", 64))
	bob := client(t, ts.URL, strings.Repeat("b", 64))
	anon := client(t, ts.URL, "")

	archive := "v1/s/0123456789abcdef0123456789abcdef"
	entry := "v1/c/" + strings.Repeat("1", 32) + "/" + strings.Repeat("2", 32)

	// Writes need a bearer; the first writer owns the name.
	if err := anon.Put(ctx, archive, []byte("x")); err == nil {
		t.Error("anonymous write accepted")
	}
	if err := alice.Put(ctx, archive, []byte("sealed archive")); err != nil {
		t.Fatal(err)
	}
	if err := bob.Put(ctx, archive, []byte("overwrite")); err == nil {
		t.Error("another client overwrote an archive")
	}
	if err := alice.Put(ctx, archive, []byte("sealed archive v2")); err != nil {
		t.Errorf("owner couldn't update: %v", err)
	}

	// Archives are readable by anyone who knows the name.
	got, err := anon.Get(ctx, archive)
	if err != nil || !bytes.Equal(got, []byte("sealed archive v2")) {
		t.Errorf("anonymous archive read: %q %v", got, err)
	}

	// Catalogs are private to their owner, and their existence isn't revealed.
	if err := alice.Put(ctx, entry, []byte("sealed entry")); err != nil {
		t.Fatal(err)
	}
	if _, err := bob.Get(ctx, entry); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("another client read a catalog entry: %v", err)
	}
	if _, err := anon.Get(ctx, entry); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("anonymous catalog read: %v", err)
	}
	prefix := "v1/c/" + strings.Repeat("1", 32) + "/"
	if names, err := alice.List(ctx, prefix); err != nil || !reflect.DeepEqual(names, []string{entry}) {
		t.Errorf("owner list = %v, %v", names, err)
	}
	if names, err := bob.List(ctx, prefix); err != nil || len(names) != 0 {
		t.Errorf("another client listed a catalog: %v, %v", names, err)
	}
	if err := bob.Put(ctx, prefix+strings.Repeat("3", 32), []byte("x")); err == nil {
		t.Error("another client wrote into a vault's catalog")
	}
	if _, err := alice.List(ctx, "v1/s/"); err == nil {
		t.Error("listing archives was allowed")
	}
	if _, err := alice.Get(ctx, "v1/s/"+strings.Repeat("f", 32)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing archive: %v", err)
	}
}
