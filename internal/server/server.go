// Package server is a storage server for sesh that only ever sees
// ciphertext. Clients encrypt everything before upload; the server stores
// opaque objects and enforces who may overwrite and list them.
//
// Access rules, by object name:
//
//	v1/s/<id>          session archive. Anyone may read it: the name is
//	                   unguessable and the content is encrypted under the
//	                   share token, which the server never sees. Only the
//	                   bearer that first wrote it may overwrite it.
//	v1/c/<vault>/<id>  catalog entry. Readable, writable and listable only by
//	                   the bearer that first wrote anything in that vault.
//
// A bearer is identified by the SHA-256 of its token, so the server stores
// no secrets.
package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/bfulton/sesh/internal/store"
)

// MaxObject is the largest object accepted.
const MaxObject = 256 << 20

// Server serves objects from a directory.
type Server struct {
	objects *store.Dir
	owners  string // directory of owner files, one per scope
	mu      sync.Mutex
}

// New returns a server storing under dir.
func New(dir string) (*Server, error) {
	s := &Server{objects: &store.Dir{Root: filepath.Join(dir, "objects")}, owners: filepath.Join(dir, "owners")}
	if err := os.MkdirAll(s.owners, 0o700); err != nil {
		return nil, err
	}
	return s, nil
}

// scope is the unit of ownership for a name: the archive itself, or the
// whole vault for catalog entries.
func scope(name string) (string, bool) {
	parts := strings.Split(name, "/")
	switch {
	case len(parts) == 3 && parts[0] == "v1" && parts[1] == "s":
		return name, true
	case len(parts) >= 3 && parts[0] == "v1" && parts[1] == "c":
		return strings.Join(parts[:3], "/"), true
	}
	return "", false
}

func bearerHash(r *http.Request) string {
	h := r.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(h, "Bearer ")
	if !ok || len(tok) < 32 {
		return ""
	}
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func (s *Server) ownerFile(sc string) string {
	return filepath.Join(s.owners, strings.ReplaceAll(sc, "/", "_"))
}

// owns reports whether bearer owns scope sc. With claim, an unowned scope is
// claimed for bearer.
func (s *Server) owns(sc, bearer string, claim bool) (bool, error) {
	if bearer == "" {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.ownerFile(sc))
	switch {
	case err == nil:
		return subtle.ConstantTimeCompare(data, []byte(bearer)) == 1, nil
	case !errors.Is(err, os.ErrNotExist):
		return false, err
	case !claim:
		return false, nil
	}
	return true, os.WriteFile(s.ownerFile(sc), []byte(bearer), 0o600)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/v1/list" && r.Method == http.MethodGet:
		s.list(w, r)
	case strings.HasPrefix(r.URL.Path, "/v1/o/"):
		name := strings.TrimPrefix(r.URL.Path, "/v1/o/")
		sc, ok := scope(name)
		if !ok || store.CheckName(name) != nil {
			http.Error(w, "invalid object name", http.StatusBadRequest)
			return
		}
		switch r.Method {
		case http.MethodGet:
			s.get(w, r, name, sc)
		case http.MethodPut:
			s.put(w, r, name, sc)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	case r.URL.Path == "/healthz":
		io.WriteString(w, "ok\n")
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) get(w http.ResponseWriter, r *http.Request, name, sc string) {
	if strings.HasPrefix(name, "v1/c/") {
		ok, err := s.owns(sc, bearerHash(r), false)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !ok { // not found, not forbidden: don't reveal that a vault exists
			http.NotFound(w, r)
			return
		}
	}
	data, err := s.objects.Get(r.Context(), name)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(data)
}

func (s *Server) put(w http.ResponseWriter, r *http.Request, name, sc string) {
	bearer := bearerHash(r)
	if bearer == "" {
		http.Error(w, "a bearer token is required to write", http.StatusUnauthorized)
		return
	}
	ok, err := s.owns(sc, bearer, true)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "this name belongs to another client", http.StatusForbidden)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxObject))
	if err != nil {
		http.Error(w, "object too large", http.StatusRequestEntityTooLarge)
		return
	}
	if err := s.objects.Put(r.Context(), name, data); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// list serves names within one vault's catalog, to its owner only.
func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	parts := strings.Split(prefix, "/")
	if len(parts) < 3 || parts[0] != "v1" || parts[1] != "c" || parts[2] == "" {
		http.Error(w, "listing is only allowed within a vault's catalog", http.StatusBadRequest)
		return
	}
	sc := strings.Join(parts[:3], "/")
	ok, err := s.owns(sc, bearerHash(r), false)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if !ok {
		return // an empty list: don't reveal whether the vault exists
	}
	names, err := s.objects.List(r.Context(), prefix)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	for _, n := range names {
		io.WriteString(w, n+"\n")
	}
}
