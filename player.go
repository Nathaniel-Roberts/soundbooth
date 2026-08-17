package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type waveReadyMsg struct {
	file string
	cols []waveCol
	dur  float64
	err  error
}

type playTickMsg struct{}

// probeDuration asks soxi for the file length in seconds.
func probeDuration(file string) (float64, error) {
	soxi, err := findBin("soxi")
	if err != nil {
		return 0, err
	}
	out, err := exec.Command(soxi, "-D", file).Output()
	if err != nil {
		return 0, err
	}
	return strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
}

// decodeWave renders an entire file into subCols pooled waveform columns
// for the seekable player view.
func decodeWave(file string, subCols int) tea.Cmd {
	return func() tea.Msg {
		dur, err := probeDuration(file)
		if err != nil || dur <= 0 {
			return waveReadyMsg{file: file, err: fmt.Errorf("probe: %v", err)}
		}
		soxPath, err := findBin("sox")
		if err != nil {
			return waveReadyMsg{file: file, err: err}
		}
		// ~100 samples pooled per column, clamped to sane decode rates
		rate := int(float64(subCols) * 100 / dur)
		if rate < 200 {
			rate = 200
		}
		if rate > 8000 {
			rate = 8000
		}
		out, err := exec.Command(soxPath, file, "-t", "raw", "-e", "signed",
			"-b", "16", "-c", "1", "-r", strconv.Itoa(rate), "-").Output()
		if err != nil {
			return waveReadyMsg{file: file, err: fmt.Errorf("decode: %v", err)}
		}
		samples := len(out) / 2
		if samples == 0 || subCols < 1 {
			return waveReadyMsg{file: file, err: fmt.Errorf("empty decode")}
		}
		per := samples / subCols
		if per < 1 {
			per = 1
		}
		reader := bytes.NewReader(out)
		cols := make([]waveCol, 0, subCols)
		var sum2, peak float64
		n := 0
		var s int16
		for i := 0; i < samples; i++ {
			if err := binary.Read(reader, binary.LittleEndian, &s); err != nil {
				break
			}
			v := float64(s) / 32768.0
			av := math.Abs(v)
			if av > peak {
				peak = av
			}
			sum2 += v * v
			if n++; n >= per && len(cols) < subCols {
				cols = append(cols, waveCol{
					rms:  lvl(math.Sqrt(sum2 / float64(n))),
					peak: lvl(peak),
				})
				n, sum2, peak = 0, 0, 0
			}
		}
		return waveReadyMsg{file: file, cols: cols, dur: dur}
	}
}

// startPlayback plays file from offset seconds via sox to the default
// output device.
func startPlayback(file string, offset float64) (*exec.Cmd, error) {
	soxPath, err := findBin("sox")
	if err != nil {
		return nil, err
	}
	if offset < 0 {
		offset = 0
	}
	cmd := exec.Command(soxPath, "-q", file, "-d", "trim", fmt.Sprintf("%.2f", offset))
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

func playTick() tea.Cmd {
	return tea.Tick(250*time.Millisecond, func(time.Time) tea.Msg { return playTickMsg{} })
}

// loadMarkersFromFile parses a "-markers.txt" file ("marker N MM:SS" or
// "marker N H:MM:SS" lines) back into durations.
func loadMarkersFromFile(audioFile string) []time.Duration {
	path := strings.TrimSuffix(audioFile, filepath.Ext(audioFile)) + "-markers.txt"
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []time.Duration
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		parts := strings.Split(fields[len(fields)-1], ":")
		secs := 0
		ok := true
		for _, p := range parts {
			v, err := strconv.Atoi(p)
			if err != nil {
				ok = false
				break
			}
			secs = secs*60 + v
		}
		if ok {
			out = append(out, time.Duration(secs)*time.Second)
		}
	}
	return out
}
