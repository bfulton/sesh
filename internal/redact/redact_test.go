package redact

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Fake secrets are made at run time, so none sits in the source for a
// scanner to flag; a fixed seed keeps tests repeatable.
type fakes struct{ r *rand.Rand }

func newFakes() fakes { return fakes{rand.New(rand.NewSource(7))} }

func (f fakes) chars(set string, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = set[f.r.Intn(len(set))]
	}
	return string(b)
}

const alnum = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func (f fakes) github() string { return "ghp_" + f.chars(alnum, 36) }
func (f fakes) aws() string    { return "AKIA" + f.chars("ABCDEFGHIJKLMNOPQRSTUVWXYZ234567", 16) }
func (f fakes) privateKey() string {
	var b strings.Builder
	b.WriteString("-----BEGIN RSA PRIVATE KEY-----\n")
	for i := 0; i < 25; i++ {
		b.WriteString(f.chars(alnum+"+/", 64) + "\n")
	}
	b.WriteString("-----END RSA PRIVATE KEY-----")
	return b.String()
}

func newPass(t *testing.T, c *Cache, deadline time.Time) *Pass {
	t.Helper()
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	return NewPass(context.Background(), r, c, deadline)
}

func TestTextRedactsSecrets(t *testing.T) {
	f := newFakes()
	gh, aws, key := f.github(), f.aws(), f.privateKey()
	in := "notes\nexport GITHUB_TOKEN=" + gh + "\naws_access_key_id = " + aws + "\n" + key + "\ndone\n"
	p := newPass(t, nil, time.Time{})
	out, err := p.Text("memory/notes.md", []byte(in))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for name, secret := range map[string]string{"github": gh, "aws": aws, "private key": key[40:100]} {
		if strings.Contains(s, secret) {
			t.Errorf("the %s secret is still there", name)
		}
	}
	for _, rule := range []string{"github-pat", "aws-access-token", "private-key"} {
		if !strings.Contains(s, "[REDACTED:"+rule+"]") {
			t.Errorf("no %s placeholder in:\n%s", rule, s)
		}
	}
	if strings.Count(s, "\n") != strings.Count(in, "\n") {
		t.Errorf("line count changed: %d to %d", strings.Count(in, "\n"), strings.Count(s, "\n"))
	}
	if !strings.HasPrefix(s, "notes\n") || !strings.HasSuffix(s, "\ndone\n") {
		t.Errorf("text around the secrets changed:\n%s", s)
	}
	if p.Redacted["memory/notes.md"] != 3 {
		t.Errorf("Redacted = %v, want 3 for memory/notes.md", p.Redacted)
	}

	// Redacting again finds nothing: placeholders aren't secrets.
	again, err := newPass(t, nil, time.Time{}).Text("x", out)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != s {
		t.Errorf("redacting twice changed the text")
	}
}

func TestCleanTextUnchanged(t *testing.T) {
	in := "func main() {\n\tfmt.Println(\"hello\")\n}\nThe key idea: tokens are counted per turn.\n"
	p := newPass(t, nil, time.Time{})
	out, err := p.Text("a", []byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != in || len(p.Redacted) != 0 {
		t.Errorf("clean text changed: %q, %v", out, p.Redacted)
	}
}

func TestJSONLSecretAfterEscapedNewline(t *testing.T) {
	aws := newFakes().aws()
	clean := `{"type":"assistant","message":{"content":[{"type":"text","text":"Done <ok> &   fine"}]},"n":1.50}` + "\n"
	dirty, _ := json.Marshal(map[string]any{"type": "user", "toolUseResult": map[string]any{"stdout": "keys:\n" + aws + "\nend"}})
	in := clean + string(dirty) + "\n" + clean

	// In the raw line the key follows the n of an escaped newline, so
	// gitleaks' word boundary fails: scanning raw JSON misses it.
	r, _ := New()
	if ts, _ := r.scan(context.Background(), string(dirty), nil); len(ts) != 0 {
		t.Errorf("scanning the raw line found %d secrets: gitleaks changed, and docs/design.md's reason for scanning decoded strings needs a look", len(ts))
	}

	p := newPass(t, nil, time.Time{})
	out, err := p.JSONL("transcript.jsonl", []byte(in))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(out), "\n")
	if len(lines) != 4 || lines[3] != "" {
		t.Fatalf("got %d lines", len(lines))
	}
	if lines[0] != clean || lines[2] != clean {
		t.Errorf("clean lines changed:\n%s%s", lines[0], lines[2])
	}
	if strings.Contains(lines[1], aws) {
		t.Fatal("the secret is still there")
	}
	var v struct {
		ToolUseResult struct{ Stdout string } `json:"toolUseResult"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &v); err != nil {
		t.Fatalf("redacted line isn't JSON: %v", err)
	}
	if v.ToolUseResult.Stdout != "keys:\n[REDACTED:aws-access-token]\nend" {
		t.Errorf("stdout = %q", v.ToolUseResult.Stdout)
	}
	if p.Redacted["transcript.jsonl"] != 1 {
		t.Errorf("Redacted = %v", p.Redacted)
	}
}

func TestJSONKeyGivesContext(t *testing.T) {
	// The value alone looks like any identifier; its key makes it a secret.
	val := newFakes().chars(alnum, 32)
	line, _ := json.Marshal(map[string]any{"input": map[string]any{"api_key": val}})
	out, err := newPass(t, nil, time.Time{}).JSONL("t", line)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), val) {
		t.Errorf("the secret is still there: %s", out)
	}
	if !strings.Contains(string(out), `"api_key":"[REDACTED:`) {
		t.Errorf("key or placeholder missing: %s", out)
	}
}

func TestEncodedSecret(t *testing.T) {
	gh := newFakes().github()
	blob := base64.StdEncoding.EncodeToString([]byte("export GITHUB_TOKEN=" + gh + "\n"))
	in := "config: " + blob + "\nother: value\n"
	p := newPass(t, nil, time.Time{})
	out, err := p.Text("f", []byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), blob[8:40]) {
		t.Errorf("the encoded secret is still there:\n%s", out)
	}
	if !strings.HasPrefix(string(out), "config: ") || !strings.HasSuffix(string(out), "\nother: value\n") {
		t.Errorf("text around the secret changed:\n%s", out)
	}
}

func TestSecretAcrossChunks(t *testing.T) {
	key := newFakes().privateKey()
	var b strings.Builder
	for i := 0; b.Len() < chunkSize-len(key)/2; i++ {
		fmt.Fprintf(&b, "line %d of ordinary output\n", i)
	}
	b.WriteString(key + "\n")
	for i := 0; b.Len() < 3*chunkSize; i++ {
		fmt.Fprintf(&b, "more %d\n", i)
	}
	in := b.String()
	if chunkEnd(in, 0) >= strings.Index(in, "-----END") {
		t.Fatal("the key doesn't cross the first chunk boundary")
	}
	out, err := newPass(t, nil, time.Time{}).Text("f", []byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), key[40:100]) || !strings.Contains(string(out), "[REDACTED:private-key]") {
		t.Error("the private key across chunks wasn't redacted")
	}
}

func TestPatchRedactsAddedLines(t *testing.T) {
	f := newFakes()
	old, added := f.github(), f.github()
	key := f.privateKey()
	keyLines := strings.Split(key, "\n")
	patch := "diff --git a/.env b/.env\nindex 1..2 100644\n--- a/.env\n+++ b/.env\n" +
		fmt.Sprintf("@@ -1,2 +1,%d @@\n", 2+len(keyLines)) +
		" OLD_TOKEN=" + old + "\n" +
		"-NAME=a\n" +
		"+NAME=b\n" +
		"+NEW_TOKEN=" + added + "\n" +
		"+" + strings.Join(keyLines, "\n+") + "\n"
	p := newPass(t, nil, time.Time{})
	out, err := p.Patch("worktree/patches/.env.patch", []byte(patch))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, added) || strings.Contains(s, key[40:100]) {
		t.Errorf("an added secret is still there:\n%s", s)
	}
	if !strings.Contains(s, " OLD_TOKEN="+old+"\n") {
		t.Error("a context line changed; the patch would no longer apply")
	}
	if strings.Count(s, "\n") != strings.Count(patch, "\n") {
		t.Errorf("line count changed:\n%s", s)
	}
	for _, l := range strings.Split(strings.TrimSuffix(s, "\n"), "\n")[5:] {
		if l == "" || !strings.Contains(" -+", l[:1]) {
			t.Errorf("hunk line %q lost its prefix", l)
		}
	}
	bin := []byte("diff --git a/x b/x\nGIT binary patch\nliteral 3\n" + added + "\n")
	if out, _ := p.Patch("x", bin); string(out) != string(bin) {
		t.Error("a binary patch changed")
	}
}

func TestCacheSkipsCleanContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scan")
	gh := newFakes().github()
	line := func(s string) string { b, _ := json.Marshal(map[string]string{"text": s}); return string(b) + "\n" }
	transcript := line("first turn, nothing secret") + line("token "+gh) + line("second turn")
	text := strings.Repeat("plain output line\n", 3000)

	first := newPass(t, OpenCache(path), time.Time{})
	out1, err := first.JSONL("t", []byte(transcript))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Text("f", []byte(text)); err != nil {
		t.Fatal(err)
	}
	if err := first.Finish(); err != nil {
		t.Fatal(err)
	}

	// A later save scans only what is new: the line with a secret comes
	// from the cache's redacted copy.
	second := newPass(t, OpenCache(path), time.Time{})
	out2, err := second.JSONL("t", []byte(transcript+line("third turn")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Text("f", []byte(text)); err != nil {
		t.Fatal(err)
	}
	if err := second.Finish(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(out2), string(out1)) || strings.Contains(string(out2), gh) {
		t.Error("the second save redacted differently")
	}
	// Lines count without their newlines.
	if want := int64(len(line("third turn")) - 1); second.scanned != want {
		t.Errorf("second save scanned %d bytes, want %d", second.scanned, want)
	}
	if second.Redacted["t"] != 1 {
		t.Errorf("second save counted %v, want the cached line's placeholder", second.Redacted)
	}
	// The cache keeps the redacted copy, never the secret.
	copies, _ := os.ReadDir(path + ".d")
	if len(copies) != 1 {
		t.Fatalf("%d redacted copies, want 1", len(copies))
	}
	if b, _ := os.ReadFile(filepath.Join(path+".d", copies[0].Name())); strings.Contains(string(b), gh) || !strings.Contains(string(b), "[REDACTED:") {
		t.Errorf("redacted copy: %s", b)
	}
	if b, _ := os.ReadFile(path); strings.Contains(string(b), gh) {
		t.Error("the cache holds the secret")
	}

	// Under other rules, the cache is ignored.
	c := OpenCache(path)
	c.reset([16]byte{1})
	if len(c.old) != 0 {
		t.Error("a cache from other rules was used")
	}
}

func TestDeadlineLeavesPassIncomplete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scan")
	line := `{"text":"a turn"}` + "\n"
	first := newPass(t, OpenCache(path), time.Time{})
	if _, err := first.JSONL("t", []byte(line)); err != nil {
		t.Fatal(err)
	}
	if err := first.Finish(); err != nil {
		t.Fatal(err)
	}

	next := `{"text":"the next turn"}` + "\n"
	late := newPass(t, OpenCache(path), time.Now().Add(-time.Second))
	if _, err := late.JSONL("t", []byte(line+next)); err != nil {
		t.Fatal(err)
	}
	var inc *Incomplete
	if err := late.Finish(); !errors.As(err, &inc) || inc.Left != int64(len(next)-1) || inc.Scanned != 0 {
		t.Fatalf("Finish = %v, want Incomplete with %d bytes left", err, len(next)-1)
	}
	// The incomplete pass kept what was cached.
	again := newPass(t, OpenCache(path), time.Time{})
	if _, err := again.JSONL("t", []byte(line+next)); err != nil {
		t.Fatal(err)
	}
	if again.scanned != int64(len(next)-1) {
		t.Errorf("scanned %d bytes, want only the new line", again.scanned)
	}
	if err := again.Finish(); err != nil {
		t.Fatal(err)
	}
}

func TestSpan(t *testing.T) {
	s := "ab\ncdef\ngh"
	for _, c := range []struct{ sl, sc, el, ec, start, end int }{
		{0, 1, 0, 2, 0, 2}, // "ab": line 0 counts from 1
		{1, 2, 1, 5, 3, 7}, // "cdef": later lines count from their newline
		{1, 3, 2, 2, 4, 9}, // "def\ng"
	} {
		start, end, ok := span(s, c.sl, c.sc, c.el, c.ec)
		if !ok || start != c.start || end != c.end {
			t.Errorf("span(%d,%d,%d,%d) = %d,%d,%v, want %d,%d", c.sl, c.sc, c.el, c.ec, start, end, ok, c.start, c.end)
		}
	}
}

func TestJSONKeysRedacted(t *testing.T) {
	f := newFakes()
	a, b := f.github(), f.github()
	line, _ := json.Marshal(map[string]any{"input": map[string]any{a: "one", b: "two", "plain": "three"}})
	p := newPass(t, nil, time.Time{})
	out, err := p.JSONL("t", line)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), a) || strings.Contains(string(out), b) {
		t.Fatalf("a secret key is still there: %s", out)
	}
	var v struct{ Input map[string]string }
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatal(err)
	}
	// Both keys become placeholders without one value overwriting the other.
	if len(v.Input) != 3 || v.Input["plain"] != "three" || p.Redacted["t"] != 2 {
		t.Errorf("input = %v, Redacted = %v", v.Input, p.Redacted)
	}
}

func TestLongKeyAcrossChunks(t *testing.T) {
	// A PGP private key with subkeys runs past the 8 KB overlap.
	f := newFakes()
	var key strings.Builder
	key.WriteString("-----BEGIN PGP PRIVATE KEY BLOCK-----\n\n")
	for key.Len() < 3*overlap {
		key.WriteString(f.chars(alnum+"+/", 64) + "\n")
	}
	key.WriteString("-----END PGP PRIVATE KEY BLOCK-----")
	var b strings.Builder
	for b.Len() < chunkSize-overlap/2 {
		b.WriteString("ordinary output\n")
	}
	b.WriteString(key.String() + "\nafter\n")
	in := b.String()
	if strings.Index(in, "-----BEGIN") > chunkSize || strings.Index(in, "-----END")-strings.Index(in, "-----BEGIN") < overlap+chunkSize/2 {
		t.Fatal("the key doesn't start in the first chunk and outrun the overlap")
	}
	p := newPass(t, nil, time.Time{})
	out, err := p.Text("f", []byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), key.String()[100:200]) || !strings.Contains(string(out), "[REDACTED:private-key]") {
		t.Error("a key longer than the overlap, across chunks, wasn't redacted")
	}
	if !strings.HasSuffix(string(out), "\nafter\n") || strings.Count(string(out), "\n") != strings.Count(in, "\n") {
		t.Error("text around the key changed")
	}
}

func TestDeadlineInsideJSONString(t *testing.T) {
	// The deadline is checked between the windows of one long string, not
	// only between lines.
	p := newPass(t, nil, time.Now().Add(-time.Second))
	long := strings.Repeat("output line\n", 5000)
	n := 0
	if _, err := p.walk(object{{"text", long}}, "", &n); err != nil {
		t.Fatal(err)
	}
	if p.left < int64(len(long)) {
		t.Errorf("%d bytes left after the deadline, want the whole string's %d", p.left, len(long))
	}
}

func TestCountsEveryPlaceholder(t *testing.T) {
	gh := newFakes().github()
	p := newPass(t, nil, time.Time{})
	out, err := p.Text("f", []byte("first "+gh+"\nagain "+gh+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(out), "[REDACTED:"); got != 2 || p.Redacted["f"] != 2 {
		t.Errorf("%d placeholders, Redacted = %v; want 2 and 2", got, p.Redacted)
	}
}

func TestLongEncodedKeyAcrossChunks(t *testing.T) {
	// A key whose base64 form, unwrapped, is longer than a chunk and its
	// overlap together, such as a large PGP key in a Kubernetes secret.
	f := newFakes()
	var key strings.Builder
	key.WriteString("-----BEGIN PGP PRIVATE KEY BLOCK-----\n\n")
	for key.Len() < 6*overlap {
		key.WriteString(f.chars(alnum+"+/", 64) + "\n")
	}
	key.WriteString("-----END PGP PRIVATE KEY BLOCK-----\n")
	blob := base64.StdEncoding.EncodeToString([]byte(key.String()))
	var b strings.Builder
	for b.Len() < chunkSize-overlap/2 {
		b.WriteString("ordinary output\n")
	}
	b.WriteString("  tls.key: " + blob + "\nafter\n")
	out, err := newPass(t, nil, time.Time{}).Text("f", []byte(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), blob[len(blob)/2:len(blob)/2+64]) {
		t.Error("a long encoded key across chunks wasn't redacted")
	}
	if !strings.HasSuffix(string(out), "\nafter\n") {
		t.Error("text after the key changed")
	}
}

func TestImageDataNotScanned(t *testing.T) {
	// Scanning stops at the deadline; a base64 image's data isn't scanned
	// at all, so nothing of it is left.
	p := newPass(t, nil, time.Now().Add(-time.Second))
	data := strings.Repeat("iVBORw0KGgoAAAANSUhEUgAA", 4000)
	n := 0
	if _, err := p.walk(object{{"type", "image"}, {"source", object{{"type", "base64"}, {"media_type", "image/png"}, {"data", data}}}}, "", &n); err != nil {
		t.Fatal(err)
	}
	if p.left >= int64(len(data)) {
		t.Errorf("%d bytes left: the image data was scanned", p.left)
	}
}

func TestBase64TextSourceScanned(t *testing.T) {
	gh := newFakes().github()
	data := base64.StdEncoding.EncodeToString([]byte("GITHUB_TOKEN=" + gh + "\n"))
	line, _ := json.Marshal(map[string]any{"source": map[string]any{"type": "base64", "media_type": "text/plain", "data": data}})
	out, err := newPass(t, nil, time.Time{}).JSONL("t", line)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), data) {
		t.Error("base64 text/plain data was skipped")
	}
}

func TestBlockEndBounded(t *testing.T) {
	// After a PEM block's end, a chunk grows to the next line end only
	// within chunkSize, not across a long line.
	s := "-----BEGIN RSA PRIVATE KEY-----\n" + strings.Repeat("x", 100) + strings.Repeat("y", chunkSize) +
		"-----END RSA PRIVATE KEY-----" + strings.Repeat("z", 4*chunkSize)
	b := blockEnd(s, 0, 50)
	if end := strings.Index(s, "-----END"); b < end || b > end+chunkSize {
		t.Errorf("blockEnd = %d, want within a chunk after the END marker at %d", b, end)
	}
}

func TestJSONLLeavesOutLinesThatArentJSON(t *testing.T) {
	// A line still being written, with a key after an escaped newline,
	// which a raw scan misses.
	aws := newFakes().aws()
	whole := `{"text":"a turn"}` + "\n"
	partial := `{"type":"user","toolUseResult":{"stdout":"keys:\n` + aws + `\nmore`
	p := newPass(t, nil, time.Time{})
	out, err := p.JSONL("t", []byte(whole+"not json at all\n"+whole+partial))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != whole+whole {
		t.Errorf("got %q, want only the whole lines", out)
	}
}

func TestJSONLLeavesOutTrailingData(t *testing.T) {
	// After one whole value, a suffix that starts with } or ] is still not
	// JSON, and may hold a secret.
	gh := newFakes().github()
	whole := `{"text":"a turn"}` + "\n"
	for _, bad := range []string{`{"text":"a"}} ` + gh, `{"text":"a"}]` + gh, `{"text":"a"} x` + gh} {
		out, err := newPass(t, nil, time.Time{}).JSONL("t", []byte(whole+bad+"\n"))
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != whole {
			t.Errorf("a line with trailing data was kept: %q", out)
		}
	}
}

func TestJSONLDuplicateKeys(t *testing.T) {
	// A repeated key's earlier value is scanned too, and both stay.
	gh := newFakes().github()
	line := `{"type":"user","token":"` + gh + `","token":"clean"}`
	out, err := newPass(t, nil, time.Time{}).JSONL("t", []byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"type":"user","token":"[REDACTED:github-pat]","token":"clean"}`; string(out) != want {
		t.Errorf("got %s, want %s", out, want)
	}
}

func TestJSONLKeepsMemberOrder(t *testing.T) {
	gh := newFakes().github()
	line := `{"z":1,"a":{"y":true,"b":null},"m":"token ` + gh + ` <&>","n":[1.50,"x"]}`
	out, err := newPass(t, nil, time.Time{}).JSONL("t", []byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"z":1,"a":{"y":true,"b":null},"m":"token [REDACTED:github-pat] <&>","n":[1.50,"x"]}`; string(out) != want {
		t.Errorf("got %s, want %s", out, want)
	}
}

func TestCacheKeepsRedactedFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scan")
	f := newFakes()
	text := strings.Repeat("plain output line\n", 3000) + "GITHUB_TOKEN=" + f.github() + "\n"
	patch := "diff --git a/.env b/.env\n--- a/.env\n+++ b/.env\n@@ -0,0 +1 @@\n+TOKEN=" + f.github() + "\n"

	first := newPass(t, OpenCache(path), time.Time{})
	t1, err := first.Text("f", []byte(text))
	if err != nil {
		t.Fatal(err)
	}
	p1, err := first.Patch("p", []byte(patch))
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Finish(); err != nil {
		t.Fatal(err)
	}

	second := newPass(t, OpenCache(path), time.Time{})
	t2, _ := second.Text("f", []byte(text))
	p2, _ := second.Patch("p", []byte(patch))
	if err := second.Finish(); err != nil {
		t.Fatal(err)
	}
	if second.scanned != 0 {
		t.Errorf("second save scanned %d bytes, want none", second.scanned)
	}
	if string(t2) != string(t1) || string(p2) != string(p1) || second.Redacted["f"] != 1 || second.Redacted["p"] != 1 {
		t.Errorf("second save redacted differently: %v", second.Redacted)
	}

	// Once the content is gone, a complete save drops its copies.
	third := newPass(t, OpenCache(path), time.Time{})
	if _, err := third.Text("f", []byte("other\n")); err != nil {
		t.Fatal(err)
	}
	if err := third.Finish(); err != nil {
		t.Fatal(err)
	}
	if copies, _ := os.ReadDir(path + ".d"); len(copies) != 0 {
		t.Errorf("%d redacted copies left, want 0", len(copies))
	}
}
