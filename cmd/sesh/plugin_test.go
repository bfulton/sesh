package main

import (
	"encoding/json"
	"os"
	"path/filepath"
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
}

// commands returns every "command" string in a JSON value.
func commands(v any) []string {
	var out []string
	switch v := v.(type) {
	case map[string]any:
		for k, x := range v {
			if s, ok := x.(string); ok && k == "command" && strings.Contains(s, "/") {
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
