package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/bfulton/sesh/internal/sesh"
	"github.com/bfulton/sesh/internal/store"
	"github.com/bfulton/sesh/internal/token"
	"github.com/bfulton/sesh/internal/vault"
)

// Settings live in ~/.sesh (or $SESH_HOME), a folder per vault:
//
//	<vault>/config    key=value lines: "store" is the storage URL;
//	                  "redact=off" saves secrets as they are; the aws_
//	                  settings are an s3 store's credentials and region
//	<vault>/key       the vault key
//	<vault>/tokens/   share tokens for sessions saved without a vault
//	<vault>/cache/    decrypted sessions and the search index (this device only)
//	<vault>/scanned/  per session, hashes of content already scanned for
//	                  secrets, and redacted copies of what held secrets
//	<vault>/state.json what sync has already saved
//	hook-status.json, hook.lock, pending/  the plugin's hook, for every vault
//
// The vault is SESH_VAULT, else "default". SESH_STORE, SESH_KEY, SESH_REDACT
// and the SESH_AWS_ variables override its files, which is how cloud
// sessions are configured: as environment variables of the cloud
// environment.

func seshHome() string {
	if d := os.Getenv("SESH_HOME"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".sesh")
}

// defaultVault is the vault sesh uses unless SESH_VAULT names another, so a
// first setup needs no name.
const defaultVault = "default"

// vaultOverride is the vault sesh init names on its command line.
var vaultOverride string

func vaultName() string {
	if vaultOverride != "" {
		return vaultOverride
	}
	if n := os.Getenv("SESH_VAULT"); n != "" {
		return n
	}
	return defaultVault
}

// validVault matches a vault name: it names a folder, the last part of an
// s3 store's path and part of its IAM user's name.
var validVault = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

func checkVaultName(name string) error {
	if !validVault.MatchString(name) {
		return fmt.Errorf("vault name %q: use up to 32 lowercase letters, digits, - and _, starting with a letter or digit", name)
	}
	return nil
}

// vaultDir is the folder of the vault's settings and local state.
func vaultDir() string { return filepath.Join(seshHome(), vaultName()) }

func readConfig() map[string]string {
	cfg := map[string]string{}
	f, err := os.Open(filepath.Join(vaultDir(), "config"))
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

// configKeys are the settings a vault's config file holds, each with the
// variable that overrides it.
var configKeys = []struct{ key, env string }{
	{"store", "SESH_STORE"},
	{"redact", "SESH_REDACT"},
	{"aws_access_key_id", "SESH_AWS_ACCESS_KEY_ID"},
	{"aws_secret_access_key", "SESH_AWS_SECRET_ACCESS_KEY"},
	{"aws_session_token", "SESH_AWS_SESSION_TOKEN"},
	{"aws_region", "SESH_AWS_REGION"},
	{"aws_account_id", "SESH_AWS_ACCOUNT_ID"},
	{"aws_endpoint_url_s3", "SESH_AWS_ENDPOINT_URL_S3"},
	{"s3_path_style", "SESH_S3_PATH_STYLE"},
}

func writeConfig(cfg map[string]string) error {
	if err := os.MkdirAll(vaultDir(), 0o700); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# sesh settings for this vault; each SESH_ variable overrides its setting\n")
	for _, k := range configKeys {
		if v := cfg[k.key]; v != "" {
			fmt.Fprintf(&b, "%s=%s\n", k.key, v)
		}
	}
	return os.WriteFile(filepath.Join(vaultDir(), "config"), []byte(b.String()), 0o600)
}

// setting returns a setting: its variable if set, else the config file's.
func setting(key string) string {
	for _, k := range configKeys {
		if k.key == key {
			if v := os.Getenv(k.env); v != "" {
				return v
			}
		}
	}
	return readConfig()[key]
}

// loadVault returns the vault from SESH_KEY or ~/.sesh/key, or nil if there is none.
func loadVault() (*vault.Vault, error) {
	s := os.Getenv("SESH_KEY")
	if s == "" {
		b, err := os.ReadFile(filepath.Join(vaultDir(), "key"))
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

func storeURL() string { return setting("store") }

// s3Config is an s3 store's settings.
func s3Config() store.S3Config {
	return store.S3Config{
		AccessKeyID:     setting("aws_access_key_id"),
		SecretAccessKey: setting("aws_secret_access_key"),
		SessionToken:    setting("aws_session_token"),
		Region:          setting("aws_region"),
		Endpoint:        setting("aws_endpoint_url_s3"),
		PathStyle:       setting("s3_path_style") != "",
		AccountID:       setting("aws_account_id"),
	}
}

// checkAllBuckets has openStore check S3 stores other than AWS's, for tests.
var checkAllBuckets bool

// openStore opens the configured store. For a sesh server it authenticates
// with the token derived from the vault key, if there is one.
func openStore() (store.Store, error) {
	u := storeURL()
	if u == "" {
		return nil, errors.New("no storage configured: run sesh init, or set SESH_STORE")
	}
	st, err := store.Open(u, s3Config())
	if err != nil {
		return nil, err
	}
	// An AWS bucket must not be public, which may have changed since init.
	// Other S3-compatible stores have no Block Public Access to ask about.
	if s, ok := st.(*store.S3); ok && (setting("aws_endpoint_url_s3") == "" || checkAllBuckets) {
		if err := s.CheckPrivate(context.Background()); err != nil {
			return nil, err
		}
	}
	if r, ok := st.(*store.Remote); ok {
		if v, _ := loadVault(); v != nil {
			r.Token = v.ServerToken()
		}
	}
	return st, nil
}

func tokenDir() string { return filepath.Join(vaultDir(), "tokens") }

func cacheDir() string { return filepath.Join(vaultDir(), "cache") }

// keepSecrets reports whether saves keep secrets as they are, instead of
// replacing them with placeholders: SESH_REDACT=off, or redact=off in the
// config file.
func keepSecrets() bool { return setting("redact") == "off" }

// scanCache returns the file remembering what of a session was scanned for
// secrets. It's named by a hash of the session id, which may hold characters
// a file name can't, such as : on Windows.
func scanCache(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(sessionID))
	return filepath.Join(vaultDir(), "scanned", hex.EncodeToString(sum[:16]))
}

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
