package main

import (
	"os"
	"path/filepath"
	"strings"
)

// searchHit is one matched transcript line.
type searchHit struct {
	Audio   string // flac path the transcript belongs to
	File    string // transcript file the hit is in
	LineNo  int
	Snippet string
}

// transcriptFileFor picks the best transcript text for a recording:
// the named markdown if present, else whisper's plain txt.
func transcriptFileFor(audio string) string {
	stem := strings.TrimSuffix(audio, filepath.Ext(audio))
	base := audioStem(audio)
	md := filepath.Join(stem, base+"-transcript.md")
	if _, err := os.Stat(md); err == nil {
		return md
	}
	txt := filepath.Join(stem, base+".txt")
	if _, err := os.Stat(txt); err == nil {
		return txt
	}
	return ""
}

// searchTranscripts greps every transcript in the library for query,
// case-insensitive. Caps: 3 hits per recording, 60 overall.
func searchTranscripts(entries []libEntry, query string) []searchHit {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return nil
	}
	var hits []searchHit
	for _, e := range entries {
		if !e.HasTx {
			continue
		}
		tf := transcriptFileFor(e.Path)
		if tf == "" {
			continue
		}
		data, err := os.ReadFile(tf)
		if err != nil {
			continue
		}
		perFile := 0
		for i, line := range strings.Split(string(data), "\n") {
			if !strings.Contains(strings.ToLower(line), query) {
				continue
			}
			snippet := strings.TrimSpace(line)
			if len(snippet) > 120 {
				// centre the snippet on the match
				idx := strings.Index(strings.ToLower(snippet), query)
				start := idx - 40
				if start < 0 {
					start = 0
				}
				end := start + 120
				if end > len(snippet) {
					end = len(snippet)
				}
				snippet = "…" + snippet[start:end] + "…"
			}
			hits = append(hits, searchHit{Audio: e.Path, File: tf, LineNo: i + 1, Snippet: snippet})
			if perFile++; perFile >= 3 {
				break
			}
		}
		if len(hits) >= 60 {
			break
		}
	}
	return hits
}

// hitContext returns the lines around a hit for the done-screen preview.
func hitContext(h searchHit, around int) []string {
	data, err := os.ReadFile(h.File)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(data), "\n")
	start := h.LineNo - 1 - around
	if start < 0 {
		start = 0
	}
	end := h.LineNo + around
	if end > len(lines) {
		end = len(lines)
	}
	var out []string
	for _, l := range lines[start:end] {
		l = strings.TrimSpace(l)
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}
