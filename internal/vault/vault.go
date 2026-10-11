// Package vault holds everything sesh derives from a person's vault key:
// a share token per session, and an encrypted catalog of their sessions.
//
// The vault key is a token (128 random bits, 26 characters). From it:
//
//	share token for a session  Derive("sesh session v1:" + session id)
//	vault id                   Bytes("sesh vault id v1", 16), hex
//	catalog entry name         v1/c/<vault id>/<hex of Bytes("sesh entry v1:" + session id, 16)>
//	catalog encryption key     Bytes("sesh catalog key v1", 32)
//	server access token        Bytes("sesh server access v1", 32), hex
//
// Because share tokens are derived, every device with the vault key saves
// a session under the same token, with no local state.
package vault

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bfulton/sesh/internal/store"
	"github.com/bfulton/sesh/internal/token"
)

// Vault is a vault key and what derives from it.
type Vault struct {
	Key token.Token
}

// ID returns the vault id: 32 hex characters that name the vault's catalog
// without revealing anything about its key.
func (v Vault) ID() string { return hex.EncodeToString(v.Key.Bytes("sesh vault id v1", 16)) }

// SessionToken returns the share token for a session, by its session id.
func (v Vault) SessionToken(session string) token.Token {
	return v.Key.Derive("sesh session v1:" + session)
}

// ServerToken returns the bearer token used with a sesh server.
func (v Vault) ServerToken() string {
	return hex.EncodeToString(v.Key.Bytes("sesh server access v1", 32))
}

// CatalogPrefix is the object-name prefix of the vault's catalog.
func (v Vault) CatalogPrefix() string { return "v1/c/" + v.ID() + "/" }

// EntryName returns the catalog object name for a session, by its session id.
func (v Vault) EntryName(session string) string {
	return v.CatalogPrefix() + hex.EncodeToString(v.Key.Bytes("sesh entry v1:"+session, 16))
}

// ArchiveName returns the object name of the archive a share token opens.
func ArchiveName(t token.Token) string { return "v1/s/" + t.ID() }

// Entry describes one saved session in the catalog.
type Entry struct {
	// Session is the session id, which identifies the session in the vault.
	// Where its transcript is kept is not part of its identity: Claude Code
	// moves a session's transcript when the session changes folder.
	Session  string    `json:"session"`
	Source   string    `json:"source"` // e.g. "claude-code"
	Title    string    `json:"title,omitempty"`
	Cwd      string    `json:"cwd,omitempty"` // working directory, as of the save
	Repo     string    `json:"repo,omitempty"`
	Branch   string    `json:"branch,omitempty"`
	Started  time.Time `json:"started,omitempty"`
	Updated  time.Time `json:"updated,omitempty"` // last activity in the session
	SavedAt  time.Time `json:"saved_at"`
	Messages int       `json:"messages"`
	Bytes    int       `json:"bytes"` // transcript size
	Token    string    `json:"token"` // share token
	Host     string    `json:"host,omitempty"`
}

// Put writes a catalog entry for its session.
func (v Vault) Put(ctx context.Context, st store.Store, e Entry) error {
	name := v.EntryName(e.ID())
	plain, err := json.Marshal(e)
	if err != nil {
		return err
	}
	sealed, err := v.seal(name, plain)
	if err != nil {
		return err
	}
	return st.Put(ctx, name, sealed)
}

// Get reads the catalog entry for a session, by its session id.
func (v Vault) Get(ctx context.Context, st store.Store, session string) (*Entry, error) {
	name := v.EntryName(session)
	sealed, err := st.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	return v.open(name, sealed)
}

// ID returns what identifies the entry's session: its session id.
func (e Entry) ID() string { return e.Session }

// Active is when the session was last active: its last message, or when it
// was saved if that is unknown.
func (e Entry) Active() time.Time {
	if !e.Updated.IsZero() {
		return e.Updated
	}
	return e.SavedAt
}

// List reads the whole catalog, most recently active first. Entries that fail to
// decrypt are skipped and counted.
func (v Vault) List(ctx context.Context, st store.Store) ([]Entry, int, error) {
	names, err := st.List(ctx, v.CatalogPrefix())
	if err != nil {
		return nil, 0, err
	}
	entries := make([]Entry, 0, len(names))
	var mu sync.Mutex
	var bad int
	var firstErr error
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, n := range names {
		wg.Add(1)
		sem <- struct{}{}
		go func(n string) {
			defer wg.Done()
			defer func() { <-sem }()
			sealed, err := st.Get(ctx, n)
			var e *Entry
			if err == nil {
				e, err = v.open(n, sealed)
			}
			mu.Lock()
			defer mu.Unlock()
			switch {
			case errors.Is(err, errDecrypt):
				bad++
			case err != nil:
				if firstErr == nil {
					firstErr = err
				}
			default:
				entries = append(entries, *e)
			}
		}(n)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, 0, firstErr
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Active().After(entries[j].Active()) })
	return entries, bad, nil
}

// Find returns the entries matching q: those whose session id starts with
// q, else those whose title contains q (case-insensitive).
func Find(entries []Entry, q string) []Entry {
	var out []Entry
	for _, e := range entries {
		if strings.HasPrefix(e.Session, q) {
			out = append(out, e)
		}
	}
	if len(out) > 0 {
		return out
	}
	lp := strings.ToLower(q)
	for _, e := range entries {
		if strings.Contains(strings.ToLower(e.Title), lp) {
			out = append(out, e)
		}
	}
	return out
}

// Handle returns a short name for e that Find resolves to e alone among
// entries: the first 8 characters of its session id, or the whole id when
// another session shares those 8 characters.
func Handle(entries []Entry, e Entry) string {
	short := e.Session
	if len(short) > 8 {
		short = short[:8]
	}
	for _, o := range entries {
		if o.ID() != e.ID() && strings.HasPrefix(o.Session, short) {
			return e.ID()
		}
	}
	return short
}

var errDecrypt = errors.New("catalog entry can't be decrypted")

// seal encrypts with AES-256-GCM, binding the ciphertext to its object name
// so an entry can't be swapped for another session's.
func (v Vault) seal(name string, plain []byte) ([]byte, error) {
	gcm, err := v.gcm()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plain, []byte(name)), nil
}

func (v Vault) open(name string, sealed []byte) (*Entry, error) {
	gcm, err := v.gcm()
	if err != nil {
		return nil, err
	}
	n := gcm.NonceSize()
	if len(sealed) < n+gcm.Overhead() {
		return nil, errDecrypt
	}
	plain, err := gcm.Open(nil, sealed[:n], sealed[n:], []byte(name))
	if err != nil {
		return nil, errDecrypt
	}
	var e Entry
	if err := json.Unmarshal(plain, &e); err != nil {
		return nil, fmt.Errorf("catalog entry %s: %w", name, err)
	}
	return &e, nil
}

func (v Vault) gcm() (cipher.AEAD, error) {
	block, err := aes.NewCipher(v.Key.Bytes("sesh catalog key v1", 32))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
