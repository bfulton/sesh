package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/bfulton/sesh/internal/sesh"
	"github.com/bfulton/sesh/internal/store"
	"github.com/bfulton/sesh/internal/token"
	"github.com/bfulton/sesh/internal/vault"
)

// Settings live in ~/.sesh (or $SESH_HOME):
//
//	config   key=value lines; "store" is the storage URL
//	key      the vault key
//	tokens/  share tokens for sessions saved without a vault
//	cache/   decrypted sessions and the search index (this device only)
//	state    what sync has already saved
//
// SESH_STORE and SESH_KEY override the files, which is how cloud sessions
// are configured: as environment variables of the cloud environment.

func seshHome() string {
	if d := os.Getenv("SESH_HOME"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".sesh")
}

func readConfig() map[string]string {
	cfg := map[string]string{}
	f, err := os.Open(filepath.Join(seshHome(), "config"))
	if err != nil {
		return cfg
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			cfg[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return cfg
}

func writeConfig(cfg map[string]string) error {
	if err := os.MkdirAll(seshHome(), 0o700); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# sesh settings; SESH_STORE and SESH_KEY override these\n")
	for _, k := range []string{"store"} {
		if v := cfg[k]; v != "" {
			fmt.Fprintf(&b, "%s=%s\n", k, v)
		}
	}
	return os.WriteFile(filepath.Join(seshHome(), "config"), []byte(b.String()), 0o600)
}

// loadVault returns the vault from SESH_KEY or ~/.sesh/key, or nil if there is none.
func loadVault() (*vault.Vault, error) {
	s := os.Getenv("SESH_KEY")
	if s == "" {
		b, err := os.ReadFile(filepath.Join(seshHome(), "key"))
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		s = string(b)
	}
	k, err := token.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("vault key: %w", err)
	}
	return &vault.Vault{Key: k}, nil
}

func requireVault() (*vault.Vault, error) {
	v, err := loadVault()
	if err == nil && v == nil {
		err = errors.New("no vault key: run sesh init, or set SESH_KEY")
	}
	return v, err
}

func storeURL() string {
	if u := os.Getenv("SESH_STORE"); u != "" {
		return u
	}
	return readConfig()["store"]
}

// openStore opens the configured store. For a sesh server it authenticates
// with the token derived from the vault key, if there is one.
func openStore() (store.Store, error) {
	u := storeURL()
	if u == "" {
		return nil, errors.New("no storage configured: run sesh init, or set SESH_STORE")
	}
	st, err := store.Open(u)
	if err != nil {
		return nil, err
	}
	if r, ok := st.(*store.Remote); ok {
		if v, _ := loadVault(); v != nil {
			r.Token = v.ServerToken()
		}
	}
	return st, nil
}

func tokenDir() string { return filepath.Join(seshHome(), "tokens") }

func budget() int {
	if n, err := strconv.Atoi(os.Getenv("SESH_BUDGET")); err == nil && n > 0 {
		return n
	}
	return sesh.DefaultBudget
}

// inClaude reports whether sesh runs inside a Claude Code session, where
// anything it prints can end up in the session's transcript.
func inClaude() bool { return os.Getenv("CLAUDECODE") != "" }

// inCloud reports whether sesh runs in a Claude Code cloud session, whose
// files don't outlast it.
func inCloud() bool { return os.Getenv("CLAUDE_CODE_REMOTE") == "true" }

// hostLabel names this device in the catalog.
func hostLabel() string {
	if inCloud() {
		return "claude-cloud"
	}
	h, _ := os.Hostname()
	return h
}

func claudeHome() string {
	home, _ := os.UserHomeDir()
	return home
}
