package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/bfulton/sesh/internal/library"
	"github.com/bfulton/sesh/internal/vault"
)

// openLibrary opens this device's copy of the vault, pulling new sessions
// from the store first when pull is set.
func openLibrary(ctx context.Context, pull bool) (*library.Library, error) {
	v, err := requireVault()
	if err != nil {
		return nil, err
	}
	if !pull {
		return library.Open(ctx, nil, v, filepath.Join(seshHome(), "cache"), false)
	}
	s, err := openStore()
	if err != nil {
		return nil, err
	}
	return library.Open(ctx, s, v, filepath.Join(seshHome(), "cache"), true)
}

func libraryFlags(name string) (*flag.FlagSet, *bool) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	offline := fs.Bool("offline", false, "use this device's copy only; don't check the store")
	return fs, offline
}

func cmdList(args []string) error {
	fs, offline := libraryFlags("list")
	n := fs.Int("n", 20, "how many sessions to show")
	fs.Parse(args)
	return listSessions(os.Stdout, *n, *offline)
}

// listSessions writes the n most recently active sessions to w.
func listSessions(w io.Writer, n int, offline bool) error {
	lib, err := openLibrary(context.Background(), !offline)
	if err != nil {
		return err
	}
	for i, e := range lib.Entries {
		if i == n {
			fmt.Fprintf(w, "… %d more\n", len(lib.Entries)-i)
			break
		}
		fmt.Fprintln(w, formatEntry(lib, e))
	}
	if len(lib.Entries) == 0 {
		fmt.Fprintln(w, "No sessions yet. Run sesh sync, or sesh save in a session.")
	}
	return nil
}

func cmdSearch(args []string) error {
	fs, offline := libraryFlags("search")
	n := fs.Int("n", 10, "how many sessions to show")
	fs.Parse(args)
	query := strings.Join(fs.Args(), " ")
	if query == "" {
		return errors.New("usage: sesh search <query>")
	}
	return searchSessions(os.Stdout, query, *n, *offline)
}

// searchSessions writes the n sessions that best match query to w.
func searchSessions(w io.Writer, query string, n int, offline bool) error {
	lib, err := openLibrary(context.Background(), !offline)
	if err != nil {
		return err
	}
	hits := lib.Search(query, n)
	if len(hits) == 0 {
		fmt.Fprintf(w, "Nothing matches %q in %d sessions.\n", query, len(lib.Entries))
		return nil
	}
	for _, h := range hits {
		fmt.Fprintln(w, formatHit(lib, h))
		fmt.Fprintln(w)
	}
	return nil
}

func cmdShow(args []string) error {
	fs, offline := libraryFlags("show")
	size := fs.Int("budget", 1<<30, "maximum size in bytes")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: sesh show <session id or title words>")
	}
	lib, err := openLibrary(context.Background(), !*offline)
	if err != nil {
		return err
	}
	e, err := pickOne(lib, fs.Arg(0))
	if err != nil {
		return err
	}
	out, err := lib.Render(e.ID(), *size)
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}

// pickOne resolves q to exactly one session, or explains the ambiguity.
func pickOne(lib *library.Library, q string) (vault.Entry, error) {
	found := lib.Find(q)
	switch len(found) {
	case 0:
		return vault.Entry{}, fmt.Errorf("no session matches %q", q)
	case 1:
		return found[0], nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d sessions match %q:", len(found), q)
	for i, e := range found {
		if i == 8 {
			b.WriteString("\n  …")
			break
		}
		fmt.Fprintf(&b, "\n  %s", formatEntry(lib, e))
	}
	return vault.Entry{}, errors.New(b.String())
}

func formatEntry(lib *library.Library, e vault.Entry) string {
	title := e.Title
	if title == "" {
		title = "(untitled)"
	}
	return fmt.Sprintf("%s  %s  %-48s %4d msgs  %s", lib.Handle(e), ago(e.Active()), clip(title, 48), e.Messages, place(e))
}

func formatHit(lib *library.Library, h library.Hit) string {
	label := map[string]string{"user": "you", "assistant": "agent", "tool_call": "tool", "tool_result": "output"}[h.Kind]
	if h.Seq < 0 {
		label = "title"
	}
	more := ""
	if h.More > 0 {
		more = fmt.Sprintf("  (+%d more)", h.More)
	}
	return fmt.Sprintf("%s\n    %s: %s%s", formatEntry(lib, h.Entry), label, h.Snippet, more)
}

// place names where a session ran: its repo, else its folder, plus the host
// when it wasn't this device.
func place(e vault.Entry) string {
	p := e.Repo
	if p != "" {
		p = strings.TrimSuffix(path.Base(p), ".git")
	} else if e.Cwd != "" {
		p = path.Base(e.Cwd)
	}
	if e.Host != "" && e.Host != hostLabel() {
		p += " @" + e.Host
	}
	return p
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "  now"
	case d < time.Hour:
		return fmt.Sprintf("%3dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%3dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%3dd", int(d.Hours()/24))
	}
}
