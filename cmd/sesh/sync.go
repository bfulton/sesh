package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bfulton/sesh/internal/sesh"
)

// syncState remembers each transcript's size and modification time when it
// was last saved, so sync only uploads sessions that changed.
type syncState map[string]fileStamp

type fileStamp struct {
	Size    int64 `json:"size"`
	ModTime int64 `json:"mtime_ns"`
}

func statePath() string { return filepath.Join(seshHome(), "state.json") }

func loadState() syncState {
	st := syncState{}
	if data, err := os.ReadFile(statePath()); err == nil {
		json.Unmarshal(data, &st)
	}
	return st
}

func (s syncState) save() error {
	data, err := json.MarshalIndent(s, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(seshHome(), 0o700); err != nil {
		return err
	}
	tmp := statePath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, statePath())
}

func stamp(path string) (fileStamp, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}, false
	}
	return fileStamp{Size: fi.Size(), ModTime: fi.ModTime().UnixNano()}, true
}

// markSynced records a transcript as saved in its current state.
func markSynced(path string) {
	if s, ok := stamp(path); ok {
		st := loadState()
		st[path] = s
		st.save()
	}
}

// localTranscripts lists every Claude Code session transcript on this device.
func localTranscripts() ([]string, error) {
	paths, err := filepath.Glob(filepath.Join(claudeHome(), ".claude", "projects", "*", "*.jsonl"))
	sort.Strings(paths)
	return paths, err
}

func cmdSync(args []string) error {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	quiet := fs.Bool("q", false, "print only errors")
	fs.Parse(args)
	n, err := syncOnce(context.Background(), *quiet)
	if err == nil && !*quiet {
		fmt.Printf("sesh: %d sessions saved\n", n)
	}
	return err
}

// syncOnce saves every local session whose transcript changed. Only the
// conversation is saved: a session's working copy belongs to the moment it
// ran, so it is captured by the Stop hook while the session is live.
func syncOnce(ctx context.Context, quiet bool) (int, error) {
	v, err := requireVault()
	if err != nil {
		return 0, err
	}
	st, err := openStore()
	if err != nil {
		return 0, err
	}
	paths, err := localTranscripts()
	if err != nil {
		return 0, err
	}
	state := loadState()
	ids := sessionIDs(paths)
	saved := 0
	var failures []string
	for _, p := range paths {
		cur, ok := stamp(p)
		if !ok || state[p] == cur {
			continue
		}
		sid := ids[p]
		res, err := sesh.Save(ctx, sesh.SaveOptions{
			Store: st, SessionID: sid, TranscriptPath: p, SkipWorktree: true,
			Vault: v, Host: hostLabel(),
		})
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", sid, err))
			continue
		}
		state[p] = cur
		saved++
		if !quiet {
			title := ""
			if res.Entry != nil {
				title = res.Entry.Title
			}
			fmt.Printf("saved %s  %s\n", sid[:min(8, len(sid))], title)
		}
	}
	if err := state.save(); err != nil {
		return saved, err
	}
	if len(failures) > 0 {
		return saved, fmt.Errorf("%d sessions failed:\n  %s", len(failures), strings.Join(failures, "\n  "))
	}
	return saved, nil
}

// sessionIDs names each transcript's session by its file name. Claude Code
// session ids are unique, but the same id can appear in two project
// folders (a session written to two folders, or a nested session that
// inherited its parent's id). Then the most recently modified keeps the id
// and the others get a suffix from their folder, so no save overwrites
// another session's.
func sessionIDs(paths []string) map[string]string {
	byID := map[string][]string{}
	for _, p := range paths {
		id := strings.TrimSuffix(filepath.Base(p), ".jsonl")
		byID[id] = append(byID[id], p)
	}
	out := map[string]string{}
	for id, ps := range byID {
		sort.Slice(ps, func(i, j int) bool {
			si, _ := stamp(ps[i])
			sj, _ := stamp(ps[j])
			return si.ModTime > sj.ModTime
		})
		for i, p := range ps {
			if i == 0 {
				out[p] = id
				continue
			}
			sum := sha256.Sum256([]byte(filepath.Base(filepath.Dir(p))))
			out[p] = id + "-" + hex.EncodeToString(sum[:3])
		}
	}
	return out
}

func cmdWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	every := fs.Duration("every", time.Minute, "how often to look for changed sessions")
	fs.Parse(args)
	fmt.Fprintf(os.Stderr, "sesh: saving changed sessions every %s\n", *every)
	for {
		if n, err := syncOnce(context.Background(), true); err != nil {
			fmt.Fprintln(os.Stderr, "sesh:", err)
		} else if n > 0 {
			fmt.Fprintf(os.Stderr, "sesh: %s saved %d sessions\n", time.Now().Format("15:04:05"), n)
		}
		time.Sleep(*every)
	}
}
