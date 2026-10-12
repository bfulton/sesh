// Package archive packs a saved session into one encrypted blob: a 12-byte
// random nonce followed by the AES-256-GCM ciphertext of a gzipped tar.
package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"
)

// Version is the archive format version.
const Version = 1

// Manifest describes a saved session.
type Manifest struct {
	Sesh    int       `json:"sesh"`
	Source  string    `json:"source"`  // runtime that produced the transcript, e.g. "claude-code"
	Session string    `json:"session"` // source session id
	SavedAt time.Time `json:"saved_at"`

	Repo    string   `json:"repo,omitempty"`    // remote URL of the source repo
	Branch  string   `json:"branch,omitempty"`  // source branch
	Base    string   `json:"base,omitempty"`    // commit the patches apply to
	Patches []string `json:"patches,omitempty"` // changed paths, each with worktree/patches/<path>.patch
	Deleted []string `json:"deleted,omitempty"` // paths removed relative to Base
	Memory  []string `json:"memory,omitempty"`  // files in the session's project memory folder, each at memory/<path>

	// SessionDir is the folder beside the source transcript where Claude Code
	// keeps the session's large tool outputs and subagent transcripts, which
	// the transcript refers to by absolute path. Files lists those kept,
	// each at session/<path>.
	SessionDir string   `json:"session_dir,omitempty"`
	Files      []string `json:"files,omitempty"`

	// Redacted counts the placeholders that replaced secrets before saving,
	// by archive file name, such as transcript.jsonl or PatchName(path).
	Redacted map[string]int `json:"redacted,omitempty"`
}

// Names of the files inside an archive.
const (
	ManifestName   = "manifest.json"
	TranscriptName = "transcript.jsonl"
	patchDir       = "worktree/patches/"
	memoryDir      = "memory/"
	filesDir       = "session/"
)

// PatchName returns the archive file name holding the patch for a repo path.
func PatchName(p string) string { return patchDir + p + ".patch" }

// MemoryName returns the archive file name holding a memory file.
func MemoryName(p string) string { return memoryDir + p }

// FileName returns the archive file name holding a session file.
func FileName(p string) string { return filesDir + p }

// Archive is a saved session in memory.
type Archive struct {
	Manifest   Manifest
	Transcript []byte
	Patches    map[string][]byte // repo path -> binary patch
	Memory     map[string][]byte // path within the project memory folder -> content
	Files      map[string][]byte // path within Manifest.SessionDir -> content
}

// Seal packs and encrypts an archive with a 32-byte key.
func Seal(a *Archive, key []byte) ([]byte, error) {
	plain, err := pack(a)
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plain, nil), nil
}

// Open decrypts and unpacks a blob produced by Seal.
func Open(blob, key []byte) (*Archive, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(blob) < gcm.NonceSize()+gcm.Overhead() {
		return nil, errors.New("archive is too short")
	}
	n := gcm.NonceSize()
	plain, err := gcm.Open(nil, blob[:n], blob[n:], nil)
	if err != nil {
		return nil, errors.New("archive can't be decrypted: wrong token or corrupted data")
	}
	return unpack(plain)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func pack(a *Archive) ([]byte, error) {
	m := a.Manifest
	m.Sesh = Version
	m.Patches = m.Patches[:0:0]
	for p := range a.Patches {
		m.Patches = append(m.Patches, p)
	}
	sort.Strings(m.Patches)
	m.Memory = m.Memory[:0:0]
	for p := range a.Memory {
		m.Memory = append(m.Memory, p)
	}
	sort.Strings(m.Memory)
	m.Files = m.Files[:0:0]
	for p := range a.Files {
		m.Files = append(m.Files, p)
	}
	sort.Strings(m.Files)
	mj, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	add := func(name string, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	if err := add(ManifestName, mj); err != nil {
		return nil, err
	}
	if err := add(TranscriptName, a.Transcript); err != nil {
		return nil, err
	}
	for _, p := range m.Patches {
		if err := add(PatchName(p), a.Patches[p]); err != nil {
			return nil, err
		}
	}
	for _, p := range m.Memory {
		if err := add(MemoryName(p), a.Memory[p]); err != nil {
			return nil, err
		}
	}
	for _, p := range m.Files {
		if err := add(FileName(p), a.Files[p]); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func unpack(plain []byte) (*Archive, error) {
	gz, err := gzip.NewReader(bytes.NewReader(plain))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg || path.Clean(h.Name) != h.Name || strings.HasPrefix(h.Name, "/") || strings.HasPrefix(h.Name, "..") {
			return nil, fmt.Errorf("archive has unexpected entry %q", h.Name)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		files[h.Name] = data
	}

	mj, ok := files[ManifestName]
	if !ok {
		return nil, errors.New("archive has no manifest")
	}
	a := &Archive{Patches: map[string][]byte{}, Memory: map[string][]byte{}, Files: map[string][]byte{}}
	if err := json.Unmarshal(mj, &a.Manifest); err != nil {
		return nil, fmt.Errorf("archive manifest: %w", err)
	}
	if a.Manifest.Sesh != Version {
		return nil, fmt.Errorf("archive format version %d is not supported", a.Manifest.Sesh)
	}
	a.Transcript = files[TranscriptName]
	for _, p := range a.Manifest.Patches {
		data, ok := files[PatchName(p)]
		if !ok {
			return nil, fmt.Errorf("archive is missing the patch for %q", p)
		}
		a.Patches[p] = data
	}
	for _, p := range a.Manifest.Memory {
		data, ok := files[memoryDir+p]
		if !ok {
			return nil, fmt.Errorf("archive is missing memory file %q", p)
		}
		a.Memory[p] = data
	}
	for _, p := range a.Manifest.Files {
		data, ok := files[filesDir+p]
		if !ok {
			return nil, fmt.Errorf("archive is missing session file %q", p)
		}
		a.Files[p] = data
	}
	return a, nil
}
