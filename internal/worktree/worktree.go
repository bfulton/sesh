// Package worktree captures a git working copy as a base commit plus one
// binary patch per changed path, and restores it exactly.
package worktree

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Capture is a working copy relative to a base commit.
type Capture struct {
	Root    string            // repo top-level directory
	Repo    string            // remote URL, if any
	Branch  string            // current branch, or "HEAD" when detached
	Base    string            // full commit id the patches apply to
	Pushed  bool              // whether Base is reachable from a remote-tracking ref
	Patches map[string][]byte // path -> binary patch against Base
	Deleted []string          // paths present in Base and absent from the working copy
}

// Take captures the working copy of the repo containing dir. Untracked files
// are included and ignored files are not. The user's index is left untouched.
func Take(dir string) (*Capture, error) {
	root, err := git(dir, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("not a git repository: %w", err)
	}
	c := &Capture{Root: root, Patches: map[string][]byte{}}
	if c.Branch, err = git(root, nil, "rev-parse", "--abbrev-ref", "HEAD"); err != nil {
		return nil, fmt.Errorf("repository has no commits: %w", err)
	}
	if u, err := git(root, nil, "remote", "get-url", remoteName(root)); err == nil {
		c.Repo = withoutCredentials(u)
	}
	if c.Base, c.Pushed, err = base(root); err != nil {
		return nil, err
	}

	// Stage everything into a private copy of the index. Copying with the
	// original mtime matters: a fresh mtime makes git treat every entry as
	// clean, so edits made in the same second as the last index write, with
	// the same size, would be missed.
	tmp, err := os.MkdirTemp("", "sesh-index-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	idx := filepath.Join(tmp, "index")
	gitIndex, err := git(root, nil, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return nil, err
	}
	if err := copyPreservingTime(gitIndex, idx); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	env := []string{"GIT_INDEX_FILE=" + idx}
	if _, err := git(root, env, "add", "-A"); err != nil {
		return nil, err
	}

	out, err := gitBytes(root, env, "diff", "--cached", "--name-status", "--no-renames", "-z", c.Base)
	if err != nil {
		return nil, err
	}
	fields := bytes.Split(bytes.TrimSuffix(out, []byte{0}), []byte{0})
	for i := 0; i+1 < len(fields); i += 2 {
		status, p := string(fields[i]), string(fields[i+1])
		if status == "D" {
			c.Deleted = append(c.Deleted, p)
			continue
		}
		patch, err := gitBytes(root, env, "diff", "--cached", "--binary", "--full-index", "--no-renames", c.Base, "--", p)
		if err != nil {
			return nil, err
		}
		c.Patches[p] = patch
	}
	sort.Strings(c.Deleted)
	return c, nil
}

// Restore recreates a captured working copy in the repo containing dir. It
// fetches the base commit if needed, checks it out on a new branch, applies
// the patches and removes deleted paths. It refuses to run on a dirty working copy.
func Restore(dir, baseCommit, branch string, patches map[string][]byte, deleted []string) error {
	root, err := git(dir, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("not a git repository: %w", err)
	}
	if status, err := git(root, nil, "status", "--porcelain"); err != nil {
		return err
	} else if status != "" {
		return errors.New("working copy has uncommitted changes; commit or stash them before restoring")
	}
	if _, err := git(root, nil, "cat-file", "-e", baseCommit+"^{commit}"); err != nil {
		if _, ferr := git(root, nil, "fetch", "--quiet", remoteName(root), baseCommit); ferr != nil {
			return fmt.Errorf("base commit %s is not available locally and could not be fetched: %w", short(baseCommit), ferr)
		}
	}
	if _, err := git(root, nil, "checkout", "--quiet", "-b", branch, baseCommit); err != nil {
		return err
	}

	paths := make([]string, 0, len(patches))
	for p := range patches {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var all bytes.Buffer
	for _, p := range paths {
		all.Write(patches[p])
	}
	if all.Len() > 0 {
		cmd := exec.Command("git", "apply", "--binary", "--whitespace=nowarn", "-")
		cmd.Dir = root
		cmd.Stdin = &all
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git apply: %v: %s", err, strings.TrimSpace(string(out)))
		}
	}
	for _, p := range deleted {
		if err := os.Remove(filepath.Join(root, filepath.FromSlash(p))); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// base picks the commit patches are taken against: the newest commit on the
// current branch that a remote already has, so a fresh clone can fetch it.
// Commits not yet pushed travel as file content. With no remote, it is HEAD.
func base(root string) (string, bool, error) {
	for _, ref := range []string{"@{upstream}", remoteName(root) + "/HEAD"} {
		if mb, err := git(root, nil, "merge-base", "HEAD", ref); err == nil {
			return mb, true, nil
		}
	}
	head, err := git(root, nil, "rev-parse", "HEAD")
	return head, false, err
}

func remoteName(root string) string {
	if r, err := git(root, nil, "config", "branch."+currentBranch(root)+".remote"); err == nil && r != "" && r != "." {
		return r
	}
	return "origin"
}

func currentBranch(root string) string {
	b, _ := git(root, nil, "rev-parse", "--abbrev-ref", "HEAD")
	return b
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func copyPreservingTime(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chtimes(dst, fi.ModTime(), fi.ModTime())
}

func git(dir string, env []string, args ...string) (string, error) {
	out, err := gitBytes(dir, env, args...)
	return strings.TrimSpace(string(out)), err
}

func gitBytes(dir string, env []string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// withoutCredentials drops the user information from a remote URL, such as
// a token in https://x-access-token:TOKEN@github.com/o/r: the URL is only
// shown, and saves are shared.
func withoutCredentials(remote string) string {
	u, err := url.Parse(remote)
	if err != nil || u.Scheme == "" || u.User == nil {
		return remote // such as scp-style git@host:path, which holds no password
	}
	u.User = nil
	return u.String()
}
