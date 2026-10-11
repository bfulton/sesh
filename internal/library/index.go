// Package library is the local, searchable copy of a vault: the catalog,
// each session's conversation, and a full-text index over them. It lives
// only on the device that built it; the store never sees plaintext.
package library

import (
	"math"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Doc is one searchable message: a user or assistant message, a tool call
// or a tool result.
type Doc struct {
	Session string
	Seq     int // position in the session's conversation
	Kind    string
	Text    string
}

type posting struct {
	doc int32
	tf  uint16
}

// Index is an in-memory BM25 index over docs.
type Index struct {
	docs     []Doc
	lengths  []uint32
	postings map[string][]posting
	avgLen   float64
}

// kindWeight favors what people said over tool output, which is long and
// repetitive and would otherwise dominate rankings.
var kindWeight = map[string]float64{"user": 1.3, "assistant": 1.0, "tool_call": 0.6, "tool_result": 0.5}

// NewIndex indexes docs.
func NewIndex(docs []Doc) *Index {
	ix := &Index{docs: docs, lengths: make([]uint32, len(docs)), postings: map[string][]posting{}}
	var total uint64
	tf := map[string]int{}
	for i, d := range docs {
		clear(tf)
		n := 0
		eachTerm(d.Text, func(t string) {
			tf[t]++
			n++
		})
		ix.lengths[i] = uint32(n)
		total += uint64(n)
		for t, c := range tf {
			if c > math.MaxUint16 {
				c = math.MaxUint16
			}
			ix.postings[t] = append(ix.postings[t], posting{doc: int32(i), tf: uint16(c)})
		}
	}
	if len(docs) > 0 {
		ix.avgLen = float64(total) / float64(len(docs))
	}
	return ix
}

// Match is a scored document.
type Match struct {
	Doc   int
	Score float64
}

// Search ranks docs against the query with BM25 (k1=1.2, b=0.75). Every
// query term must appear in a doc for it to match, unless no doc has them
// all; then docs matching most terms are ranked instead.
func (ix *Index) Search(query string) []Match {
	var terms []string
	seen := map[string]bool{}
	eachTerm(query, func(t string) {
		if !seen[t] {
			seen[t] = true
			terms = append(terms, t)
		}
	})
	if len(terms) == 0 || len(ix.docs) == 0 {
		return nil
	}
	const k1, b = 1.2, 0.75
	n := float64(len(ix.docs))
	scores := map[int32]float64{}
	hits := map[int32]int{}
	for _, t := range terms {
		ps := ix.postings[t]
		if len(ps) == 0 {
			continue
		}
		idf := math.Log(1 + (n-float64(len(ps))+0.5)/(float64(len(ps))+0.5))
		for _, p := range ps {
			tf := float64(p.tf)
			norm := tf * (k1 + 1) / (tf + k1*(1-b+b*float64(ix.lengths[p.doc])/ix.avgLen))
			scores[p.doc] += idf * norm
			hits[p.doc]++
		}
	}
	best := 0
	for _, h := range hits {
		best = max(best, h)
	}
	var out []Match
	for d, s := range scores {
		if hits[d] < best {
			continue
		}
		out = append(out, Match{Doc: int(d), Score: s * kindWeight[ix.docs[d].Kind]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Doc < out[j].Doc
	})
	return out
}

// Terms returns the normalized terms of s, in order.
func Terms(s string) []string {
	var ts []string
	eachTerm(s, func(t string) { ts = append(ts, t) })
	return ts
}

// eachTerm splits text into lowercase terms of letters and digits. It also
// splits identifiers at case changes and underscores, so "SessionToken"
// matches "session token", and folds simple plurals.
func eachTerm(s string, fn func(string)) {
	emit := func(w string) {
		w = strings.ToLower(w)
		if utf8.RuneCountInString(w) < 2 {
			return
		}
		fn(fold(w))
	}
	start := -1
	prev := rune(0)
	for i, r := range s {
		word := unicode.IsLetter(r) || unicode.IsDigit(r)
		switch {
		case !word:
			if start >= 0 {
				emit(s[start:i])
				start = -1
			}
		case start < 0:
			start = i
		case unicode.IsUpper(r) && unicode.IsLower(prev):
			emit(s[start:i])
			start = i
		}
		prev = r
	}
	if start >= 0 {
		emit(s[start:])
	}
}

// fold maps simple English plurals to their singular: "tokens" → "token",
// "queries" → "query". It leaves words ending in "ss" or "us" alone.
func fold(w string) string {
	n := len(w)
	switch {
	case n > 4 && strings.HasSuffix(w, "ies"):
		return w[:n-3] + "y"
	case n > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") && !strings.HasSuffix(w, "us") && !strings.HasSuffix(w, "is"):
		return w[:n-1]
	}
	return w
}

// Snippet returns about width bytes of text around the first query term
// found, on rune boundaries, with ellipses where it was cut.
func Snippet(text, query string, width int) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) <= width {
		return text
	}
	lower := strings.ToLower(text)
	at := -1
	for _, t := range Terms(query) {
		if i := strings.Index(lower, t); i >= 0 && (at < 0 || i < at) {
			at = i
		}
	}
	start := 0
	if at > width/3 {
		start = at - width/3
	}
	end := min(start+width, len(text))
	for start > 0 && !utf8.RuneStart(text[start]) {
		start--
	}
	for end < len(text) && !utf8.RuneStart(text[end]) {
		end++
	}
	s := text[start:end]
	if start > 0 {
		s = "…" + s
	}
	if end < len(text) {
		s += "…"
	}
	return s
}
