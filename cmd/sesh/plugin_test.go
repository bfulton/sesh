package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The plugin's manifests parse, and every command they run is the launcher
// script, which exists and is executable.
func TestPluginFiles(t *testing.T) {
	root := filepath.Join("..", "..")
	for _, f := range []string{
		".claude-plugin/marketplace.json",
		"plugin/.claude-plugin/plugin.json",
		"plugin/hooks/hooks.json",
		"plugin/.mcp.json",
	} {
		data, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatal(err)
		}
		var v any
		if err := json.Unmarshal(data, &v); err != nil {
			t.Errorf("%s: %v", f, err)
		}
		for _, cmd := range commands(v) {
			if cmd != "${CLAUDE_PLUGIN_ROOT}/scripts/sesh" {
				t.Errorf("%s runs %q, want the launcher", f, cmd)
			}
		}
	}
	fi, err := os.Stat(filepath.Join(root, "plugin", "scripts", "sesh"))
	if err != nil || fi.Mode()&0o111 == 0 {
		t.Errorf("launcher missing or not executable: %v", err)
	}

	// /sesh runs only when the user types it, and runs no shell command: its
	// arguments reach sesh as data, through the UserPromptExpansion hook.
	skill, err := os.ReadFile(filepath.Join(root, "plugin", "skills", "sesh", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"\nname: sesh\n", "\ndisable-model-invocation: true\n"} {
		if !strings.Contains(string(skill), want) {
			t.Errorf("SKILL.md lacks %q", want)
		}
	}
	if strings.Contains(string(skill), "!`") || strings.Contains(string(skill), "```!") {
		t.Error("SKILL.md runs a shell command; its arguments would reach the shell")
	}
	var hooks struct {
		Hooks map[string][]struct{ Matcher string }
	}
	data, _ := os.ReadFile(filepath.Join(root, "plugin", "hooks", "hooks.json"))
	json.Unmarshal(data, &hooks)
	if exp := hooks.Hooks["UserPromptExpansion"]; len(exp) != 1 || exp[0].Matcher != skillName {
		t.Errorf("UserPromptExpansion hooks = %+v, want one matching %q", exp, skillName)
	}
}

// The launcher runs SESH_BIN, else its own copy for its version, else
// installs one. It never runs another sesh on PATH or in ~/go/bin: other
// tools share the name.
func TestLauncherPicksItsOwnBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the launcher is a bash script")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "plugin", "scripts", "sesh"))
	if err != nil {
		t.Fatal(err)
	}

	// setup returns an environment with an impostor sesh on PATH and in
	// ~/go/bin, and a go that installs a sesh printing "installed" when
	// goInstalls, or fails.
	setup := func(t *testing.T, goInstalls bool) (env []string, data string) {
		home := t.TempDir()
		bin := filepath.Join(home, "bin")
		data = filepath.Join(home, "data")
		stub(t, filepath.Join(bin, "sesh"), "echo impostor")
		stub(t, filepath.Join(home, "go", "bin", "sesh"), "echo impostor")
		if goInstalls {
			stub(t, filepath.Join(bin, "go"), `[ "$1" = install ] || exit 1
printf '#!/bin/sh\necho installed\n' >"$GOBIN/sesh" && chmod +x "$GOBIN/sesh"`)
		} else {
			stub(t, filepath.Join(bin, "go"), "echo 'go: no network' >&2; exit 1")
		}
		return []string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + home, "CLAUDE_PLUGIN_DATA=" + data, "SESH_VERSION=main"}, data
	}
	run := func(env []string, stdin string, args ...string) (string, error) {
		cmd := exec.Command(bash, append([]string{script}, args...)...)
		cmd.Env = env
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	t.Run("SESH_BIN", func(t *testing.T) {
		env, _ := setup(t, true)
		own := filepath.Join(t.TempDir(), "sesh")
		stub(t, own, "echo own")
		out, err := run(append(env, "SESH_BIN="+own), "", "list")
		if err != nil || out != "own\n" {
			t.Errorf("got %q, %v; want own", out, err)
		}
	})
	t.Run("installed copy", func(t *testing.T) {
		env, data := setup(t, false)
		stub(t, filepath.Join(data, "bin", "sesh"), "echo plugin")
		write(t, filepath.Join(data, "bin", "version"), "main\n")
		out, err := run(env, "", "list")
		if err != nil || out != "plugin\n" {
			t.Errorf("got %q, %v; want plugin", out, err)
		}
	})
	t.Run("installs", func(t *testing.T) {
		env, data := setup(t, true)
		out, err := run(env, "", "list")
		if err != nil || out != "installed\n" {
			t.Errorf("got %q, %v; want installed", out, err)
		}
		if v, _ := os.ReadFile(filepath.Join(data, "bin", "version")); string(v) != "main\n" {
			t.Errorf("version stamp %q, want main", v)
		}
	})
	t.Run("install fails", func(t *testing.T) {
		env, _ := setup(t, false)
		out, err := run(env, "", "list")
		if err == nil || strings.Contains(out, "impostor") || !strings.Contains(out, "go: no network") || !strings.Contains(out, "SESH_BIN") {
			t.Errorf("got %q, %v; want an install error naming SESH_BIN", out, err)
		}
	})
	t.Run("install fails in a hook", func(t *testing.T) {
		env, _ := setup(t, false)
		out, err := run(env, `{"hook_event_name":"Stop"}`, "hook")
		if err != nil || out != "" {
			t.Errorf("got %q, %v; want the hook to do nothing", out, err)
		}
	})
	t.Run("install fails for /sesh", func(t *testing.T) {
		env, _ := setup(t, false)
		out, err := run(env, `{"hook_event_name":"UserPromptExpansion","command_name":"sesh:sesh"}`, "hook")
		var resp struct {
			HookSpecificOutput struct{ HookEventName, AdditionalContext string }
		}
		if err != nil || json.Unmarshal([]byte(out), &resp) != nil {
			t.Fatalf("got %q, %v; want JSON", out, err)
		}
		if c := resp.HookSpecificOutput.AdditionalContext; !strings.Contains(c, "go: no network") || !strings.Contains(c, "SESH_BIN") {
			t.Errorf("got %+v; want the install error and SESH_BIN", resp)
		}
	})
	// A release tag is downloaded from the release and checked against its
	// SHA256SUMS; anything wrong falls back to go install.
	asset := "sesh_" + runtime.GOOS + "_" + runtime.GOARCH
	release := func(t *testing.T, sum string) string {
		binary := "#!/bin/sh\necho downloaded\n"
		if sum == "" {
			sum = fmt.Sprintf("%x", sha256.Sum256([]byte(binary)))
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v9.9.9/" + asset:
				fmt.Fprint(w, binary)
			case "/v9.9.9/SHA256SUMS":
				fmt.Fprintf(w, "%s  sesh_other_arch\n%s  %s\n", strings.Repeat("0", 64), sum, asset)
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	withRelease := func(env []string, url string) []string {
		return append(env, "SESH_VERSION=v9.9.9", "SESH_RELEASES="+url)
	}
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		t.Run("downloads the release", func(t *testing.T) {
			env, data := setup(t, false)
			out, err := run(withRelease(env, release(t, "")), "", "list")
			if err != nil || out != "downloaded\n" {
				t.Errorf("got %q, %v; want the downloaded binary", out, err)
			}
			if v, _ := os.ReadFile(filepath.Join(data, "bin", "version")); string(v) != "v9.9.9\n" {
				t.Errorf("version stamp %q", v)
			}
		})
		t.Run("bad checksum", func(t *testing.T) {
			env, data := setup(t, true)
			out, err := run(withRelease(env, release(t, strings.Repeat("f", 64))), "", "list")
			if err != nil || out != "installed\n" {
				t.Errorf("got %q, %v; want go install after the checksum fails", out, err)
			}
			if log, _ := os.ReadFile(filepath.Join(data, "install.log")); !strings.Contains(string(log), "checksum") {
				t.Errorf("install.log doesn't name the checksum: %q", log)
			}
		})
		t.Run("no release", func(t *testing.T) {
			env, _ := setup(t, true)
			out, err := run(append(env, "SESH_VERSION=v8.8.8", "SESH_RELEASES="+release(t, "")), "", "list")
			if err != nil || out != "installed\n" {
				t.Errorf("got %q, %v; want go install", out, err)
			}
		})
		t.Run("no release and no go", func(t *testing.T) {
			env, _ := setup(t, false)
			out, err := run(append(env, "SESH_VERSION=v8.8.8", "SESH_RELEASES="+release(t, "")), "", "list")
			if err == nil || !strings.Contains(out, "downloading") || !strings.Contains(out, "go: no network") {
				t.Errorf("got %q, %v; want both failures reported", out, err)
			}
		})
	}
	t.Run("hook gets the event", func(t *testing.T) {
		env, data := setup(t, false)
		stub(t, filepath.Join(data, "bin", "sesh"), `echo "$1 $(cat)"`)
		write(t, filepath.Join(data, "bin", "version"), "main\n")
		out, err := run(env, `{"hook_event_name":"Stop"}`, "hook")
		if err != nil || out != "hook {\"hook_event_name\":\"Stop\"}\n" {
			t.Errorf("got %q, %v; want the event passed to sesh hook", out, err)
		}
	})
}

func stub(t *testing.T, path, body string) {
	t.Helper()
	write(t, path, "#!/bin/sh\n"+body+"\n")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// commands returns every "command" string in a JSON value.
func commands(v any) []string {
	var out []string
	switch v := v.(type) {
	case map[string]any:
		for k, x := range v {
			if s, ok := x.(string); ok && k == "command" {
				out = append(out, s)
			}
			out = append(out, commands(x)...)
		}
	case []any:
		for _, x := range v {
			out = append(out, commands(x)...)
		}
	}
	return out
}
