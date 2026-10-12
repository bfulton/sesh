package worktree

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, root, p, content string, mode fs.FileMode) {
	t.Helper()
	full := filepath.Join(root, p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(full, mode); err != nil {
		t.Fatal(err)
	}
}

// snapshot maps every non-.git, non-ignored path to its mode and content.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	ignored := map[string]bool{}
	for _, p := range strings.Split(run(t, root, "git", "ls-files", "--others", "--ignored", "--exclude-standard", "--directory"), "\n") {
		if p != "" {
			ignored[strings.TrimSuffix(p, "/")] = true
		}
	}
	got := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if rel == ".git" || ignored[filepath.ToSlash(rel)] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		var content []byte
		if fi.Mode()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			content = []byte("-> " + target)
		} else if content, err = os.ReadFile(p); err != nil {
			return err
		}
		exec := fi.Mode()&0o100 != 0
		got[filepath.ToSlash(rel)] = map[bool]string{true: "x ", false: "- "}[exec] + string(content)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func diffSnapshots(t *testing.T, want, got map[string]string) {
	t.Helper()
	for p, w := range want {
		if g, ok := got[p]; !ok {
			t.Errorf("missing %s", p)
		} else if g != w {
			t.Errorf("%s differs:\nwant %q\ngot  %q", p, trunc(w), trunc(g))
		}
	}
	for p := range got {
		if _, ok := want[p]; !ok {
			t.Errorf("unexpected %s", p)
		}
	}
}

func trunc(s string) string {
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}

// setup creates a bare remote and a source clone with one pushed commit.
func setup(t *testing.T) (remote, src string) {
	t.Helper()
	tmp := t.TempDir()
	remote = filepath.Join(tmp, "remote.git")
	src = filepath.Join(tmp, "src")
	run(t, tmp, "git", "init", "-q", "--bare", "-b", "main", remote)
	run(t, tmp, "git", "clone", "-q", remote, src)
	write(t, src, "README.md", "hello\n", 0o644)
	write(t, src, "src/app.go", "package app\n\nfunc A() {}\n", 0o644)
	write(t, src, "old/gone.txt", "to be deleted\n", 0o644)
	write(t, src, "rename/from.txt", strings.Repeat("rename me\n", 50), 0o644)
	write(t, src, "same.txt", "AAAA\n", 0o644)
	write(t, src, "script.sh", "#!/bin/sh\necho hi\n", 0o644)
	write(t, src, ".gitignore", "*.log\n", 0o644)
	run(t, src, "git", "add", "-A")
	run(t, src, "git", "commit", "-q", "-m", "base")
	run(t, src, "git", "push", "-q", "origin", "main")
	return remote, src
}

func TestCaptureRestoreExact(t *testing.T) {
	remote, src := setup(t)

	// An unpushed commit: travels as content, not history.
	write(t, src, "unpushed.txt", "committed but not pushed\n", 0o644)
	run(t, src, "git", "add", "unpushed.txt")
	run(t, src, "git", "commit", "-q", "-m", "local only")

	// Working-copy changes of every kind.
	write(t, src, "src/app.go", "package app\n\nfunc A() {}\nfunc B() {}\n", 0o644)       // edit
	write(t, src, "new/untracked.txt", "brand new\n", 0o644)                              // untracked
	write(t, src, "bin/data.bin", string([]byte{0, 1, 2, 255, 0, 0, 7}), 0o644)           // binary
	write(t, src, "README.md", "", 0o644)                                                 // truncated
	os.Remove(filepath.Join(src, "old/gone.txt"))                                         // deleted
	os.Rename(filepath.Join(src, "rename/from.txt"), filepath.Join(src, "rename/to.txt")) // renamed
	os.Chmod(filepath.Join(src, "script.sh"), 0o755)                                      // mode change
	os.Symlink("src/app.go", filepath.Join(src, "link"))                                  // symlink
	write(t, src, "debug.log", "ignored\n", 0o644)                                        // ignored: must not travel
	run(t, src, "git", "add", "src/app.go")                                               // a staged change too

	indexBefore, _ := os.ReadFile(filepath.Join(src, ".git", "index"))
	c, err := Take(src)
	if err != nil {
		t.Fatal(err)
	}
	indexAfter, _ := os.ReadFile(filepath.Join(src, ".git", "index"))
	if !bytes.Equal(indexBefore, indexAfter) {
		t.Error("Take modified the user's index")
	}
	if !c.Pushed {
		t.Error("base should be a pushed commit")
	}
	if pushed := run(t, src, "git", "rev-parse", "origin/main"); c.Base != pushed {
		t.Errorf("base = %s, want pushed origin/main %s", c.Base, pushed)
	}
	if _, ok := c.Patches["debug.log"]; ok {
		t.Error("ignored file was captured")
	}
	wantDeleted := []string{"old/gone.txt", "rename/from.txt"}
	if strings.Join(c.Deleted, ",") != strings.Join(wantDeleted, ",") {
		t.Errorf("deleted = %v, want %v", c.Deleted, wantDeleted)
	}

	dst := filepath.Join(t.TempDir(), "dst")
	run(t, filepath.Dir(dst), "git", "clone", "-q", remote, dst)
	if err := Restore(dst, c.Base, "sesh-restore", c.Patches, c.Deleted); err != nil {
		t.Fatal(err)
	}
	want := snapshot(t, src)
	delete(want, "debug.log")
	diffSnapshots(t, want, snapshot(t, dst))
}

func TestSameSecondSameSizeEdit(t *testing.T) {
	_, src := setup(t)
	// Git compares file stat data at one-second granularity. Stage a file and
	// then edit it, same size, within one wall-clock second, so the index
	// entry's stat still matches the edited file. Git catches that edit only
	// because the entry is "racy": its mtime is not older than the index
	// file's. A copy of the index with a fresh, later mtime loses that, and
	// the edit is silently missed.
	sleepToNextSecond()
	start := time.Now().Unix()
	write(t, src, "same.txt", "CCCC\n", 0o644)
	run(t, src, "git", "add", "same.txt")
	write(t, src, "same.txt", "BBBB\n", 0o644)
	if time.Now().Unix() != start {
		t.Skip("setup crossed a second boundary; the race can't be staged")
	}
	sleepToNextSecond()

	c, err := Take(src)
	if err != nil {
		t.Fatal(err)
	}
	// Check the post-image blob id in the patch's index line, not the patch
	// body: when the stat data matches, git diff reads the working-tree file
	// for the body, so a body showing BBBB can sit under a header naming the
	// stale staged blob (CCCC).
	patch := string(c.Patches["same.txt"])
	want := run(t, src, "git", "hash-object", "same.txt")
	if !strings.Contains(patch, ".."+want) {
		t.Fatalf("same-second same-size edit was missed: patch post-image is not %s (hash of BBBB):\n%s", want, patch)
	}
}

func TestRestoreRefusesDirty(t *testing.T) {
	remote, src := setup(t)
	write(t, src, "src/app.go", "changed\n", 0o644)
	c, err := Take(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "dst")
	run(t, filepath.Dir(dst), "git", "clone", "-q", remote, dst)
	write(t, dst, "README.md", "local edit\n", 0o644)
	if err := Restore(dst, c.Base, "sesh-restore", c.Patches, c.Deleted); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("err = %v, want refusal on dirty working copy", err)
	}
}

func TestNoRemote(t *testing.T) {
	dir := t.TempDir()
	run(t, dir, "git", "init", "-q", "-b", "main")
	write(t, dir, "a.txt", "a\n", 0o644)
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-q", "-m", "init")
	write(t, dir, "a.txt", "b\n", 0o644)
	c, err := Take(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Pushed {
		t.Error("base reported as pushed with no remote")
	}
	if head := run(t, dir, "git", "rev-parse", "HEAD"); c.Base != head {
		t.Errorf("base = %s, want HEAD %s", c.Base, head)
	}
	if _, ok := c.Patches["a.txt"]; !ok {
		t.Error("edit not captured")
	}
}

func sleepToNextSecond() {
	now := time.Now()
	time.Sleep(now.Truncate(time.Second).Add(time.Second).Sub(now) + 10*time.Millisecond)
}

func keys(m map[string][]byte) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

func TestWithoutCredentials(t *testing.T) {
	for in, want := range map[string]string{
		"https://x-access-token:abc123@github.com/o/r.git": "https://github.com/o/r.git",
		"https://abc123@github.com/o/r":                    "https://github.com/o/r",
		"ssh://git@host:22/o/r.git":                        "ssh://host:22/o/r.git",
		"https://github.com/o/r.git":                       "https://github.com/o/r.git",
		"git@github.com:o/r.git":                           "git@github.com:o/r.git",
		"/srv/git/r.git":                                   "/srv/git/r.git",
	} {
		if got := withoutCredentials(in); got != want {
			t.Errorf("withoutCredentials(%q) = %q, want %q", in, got, want)
		}
	}
}
