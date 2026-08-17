package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// wSeg is one diarised transcript segment from the whispermlx JSON output.
type wSeg struct {
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
	Text    string  `json:"text"`
	Speaker string  `json:"speaker"`
}

func audioStem(audioFile string) string {
	base := filepath.Base(audioFile)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// loadSegments reads the diarised segments for a transcription output dir.
func loadSegments(outDir, audioFile string) []wSeg {
	data, err := os.ReadFile(filepath.Join(outDir, audioStem(audioFile)+".json"))
	if err != nil {
		return nil
	}
	var doc struct {
		Segments []wSeg `json:"segments"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil
	}
	return doc.Segments
}

// spkStat summarises one diarised speaker.
type spkStat struct {
	ID    string
	Name  string // user-assigned; empty until named
	Quote string // longest segment, for identification
	Dur   float64
	Share float64 // 0..1 of total talk time
}

func speakerStats(segs []wSeg) []spkStat {
	byID := map[string]*spkStat{}
	var order []string
	var total float64
	quoteLen := map[string]float64{}
	for _, s := range segs {
		id := s.Speaker
		if id == "" {
			continue
		}
		st, ok := byID[id]
		if !ok {
			st = &spkStat{ID: id}
			byID[id] = st
			order = append(order, id)
		}
		d := s.End - s.Start
		st.Dur += d
		total += d
		if d > quoteLen[id] {
			quoteLen[id] = d
			st.Quote = strings.TrimSpace(s.Text)
		}
	}
	var out []spkStat
	for _, id := range order {
		out = append(out, *byID[id])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Dur > out[j].Dur })
	for i := range out {
		if total > 0 {
			out[i].Share = out[i].Dur / total
		}
	}
	return out
}

// displayName resolves a speaker label: assigned name, else Speaker N in
// talk-time order.
func displayName(stats []spkStat, id string) string {
	for i, s := range stats {
		if s.ID == id {
			if s.Name != "" {
				return s.Name
			}
			return fmt.Sprintf("Speaker %d", i+1)
		}
	}
	return id
}

func fmtClock(seconds float64) string {
	s := int(seconds)
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, (s%3600)/60, s%60)
	}
	return fmt.Sprintf("%02d:%02d", s/60, s%60)
}

// writeNamedTranscript renders the diarised segments as a readable
// markdown transcript with speaker names, merging consecutive turns and
// weaving in any markers. Returns the written path.
func writeNamedTranscript(outDir, audioFile string, segs []wSeg, stats []spkStat, markers []time.Duration) (string, error) {
	path := filepath.Join(outDir, audioStem(audioFile)+"-transcript.md")
	var b strings.Builder
	fmt.Fprintf(&b, "# Transcript — %s\n\n", audioStem(audioFile))
	fmt.Fprintf(&b, "Source: %s\n\nSpeakers: ", filepath.Base(audioFile))
	for i, s := range stats {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s (%s, %.0f%%)", displayName(stats, s.ID), fmtClock(s.Dur), s.Share*100)
	}
	b.WriteString("\n\n---\n\n")

	nextMarker := 0
	writeMarkersUpTo := func(t float64) {
		for nextMarker < len(markers) && markers[nextMarker].Seconds() <= t {
			fmt.Fprintf(&b, "**— marker %d at %s —**\n\n",
				nextMarker+1, fmtClock(markers[nextMarker].Seconds()))
			nextMarker++
		}
	}

	curSpeaker := ""
	var curStart float64
	var curText strings.Builder
	flush := func() {
		if curText.Len() == 0 {
			return
		}
		fmt.Fprintf(&b, "%s [%s]: %s\n\n",
			displayName(stats, curSpeaker), fmtClock(curStart),
			strings.TrimSpace(curText.String()))
		curText.Reset()
	}
	for _, s := range segs {
		writeMarkersUpTo(s.Start)
		if s.Speaker != curSpeaker {
			flush()
			curSpeaker = s.Speaker
			curStart = s.Start
		}
		curText.WriteString(" " + strings.TrimSpace(s.Text))
	}
	flush()
	writeMarkersUpTo(1 << 30)

	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// writeMarkersFile records marker timestamps next to the audio.
func writeMarkersFile(audioFile string, markers []time.Duration) string {
	if len(markers) == 0 {
		return ""
	}
	path := strings.TrimSuffix(audioFile, filepath.Ext(audioFile)) + "-markers.txt"
	var b strings.Builder
	for i, mk := range markers {
		fmt.Fprintf(&b, "marker %d  %s\n", i+1, fmtClock(mk.Seconds()))
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return ""
	}
	return path
}

// --- post-processing hook ---

type postDoneMsg struct{ err error }

// runPostHook executes the configured post_command with context in env.
func runPostHook(cmdStr, audio, outDir, transcriptMD, markersFile string) tea.Cmd {
	return func() tea.Msg {
		c := exec.Command("sh", "-c", cmdStr)
		c.Env = append(os.Environ(),
			"SB_AUDIO="+audio,
			"SB_TRANSCRIPT_DIR="+outDir,
			"SB_TRANSCRIPT_MD="+transcriptMD,
			"SB_MARKERS="+markersFile,
		)
		out, err := c.CombinedOutput()
		if err != nil {
			tail := strings.TrimSpace(string(out))
			if len(tail) > 200 {
				tail = tail[len(tail)-200:]
			}
			return postDoneMsg{fmt.Errorf("%v: %s", err, tail)}
		}
		return postDoneMsg{nil}
	}
}
