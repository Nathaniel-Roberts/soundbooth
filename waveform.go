package main

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// waveCol is one rendered sub-column (one meter tick = one Braille dot
// column, i.e. 50 ms per dot column, 100 ms per character cell).
type waveCol struct {
	rms, peak float64
	clip      bool
}

// brailleDot returns the bit for dot (x 0..1, y 0..3) inside a Braille cell.
var brailleBits = [2][4]int{
	{0x01, 0x02, 0x04, 0x40},
	{0x08, 0x10, 0x20, 0x80},
}

// renderWave draws the last cols of audio as an Audacity-style waveform:
// mirrored around a centre line, Braille dots for 2x4 sub-cell resolution,
// blue peak envelope, lavender RMS core, red for clipped columns, thin
// grey centre line through silence.
func renderWave(cols []waveCol, wCells, hCells int) string {
	if wCells < 1 || hCells < 1 {
		return ""
	}
	totalDots := hCells * 4
	half := totalDots / 2 // dot rows each side of centre

	// Take the newest 2*wCells sub-columns, right-aligned.
	need := wCells * 2
	if len(cols) > need {
		cols = cols[len(cols)-need:]
	}
	pad := need - len(cols)

	// Per sub-column dot heights (0..half). Envelope always >= 1 so the
	// centre line is continuous through silence.
	env := make([]int, need)
	core := make([]int, need)
	clip := make([]bool, need)
	for i, c := range cols {
		e := int(c.peak*float64(half) + 0.5)
		if e < 1 {
			e = 1
		}
		if e > half {
			e = half
		}
		k := int(c.rms*float64(half) + 0.5)
		if k > e {
			k = e
		}
		env[pad+i] = e
		core[pad+i] = k
		clip[pad+i] = c.clip
	}
	for i := 0; i < pad; i++ {
		env[i] = 1
	}

	var b strings.Builder
	for row := 0; row < hCells; row++ {
		var line strings.Builder
		var lastStyle *lipgloss.Style
		var run strings.Builder
		flush := func() {
			if run.Len() > 0 && lastStyle != nil {
				line.WriteString(lastStyle.Render(run.String()))
				run.Reset()
			}
		}
		for cx := 0; cx < wCells; cx++ {
			bits := 0
			cellCore := true // whole cell inside the RMS core?
			cellClip := false
			cellMaxEnv := 0
			for sub := 0; sub < 2; sub++ {
				ci := cx*2 + sub
				if clip[ci] {
					cellClip = true
				}
				if env[ci] > cellMaxEnv {
					cellMaxEnv = env[ci]
				}
				for dy := 0; dy < 4; dy++ {
					y := row*4 + dy
					// distance from centre: rows [0,half) above, [half,2*half) below
					var dist int
					if y < half {
						dist = half - 1 - y
					} else {
						dist = y - half
					}
					if dist < env[ci] {
						bits |= brailleBits[sub][dy]
						if dist >= core[ci] {
							cellCore = false
						}
					}
				}
			}
			style := &waveEnvStyle
			switch {
			case bits == 0:
				run.WriteRune(' ')
				continue
			case cellClip:
				style = &waveClipStyle
			case cellCore:
				style = &waveCoreStyle
			}
			// Silence renders as just the 1-dot-each-side hairline: dim it.
			if !cellClip && cellMaxEnv <= 1 {
				style = &waveMidStyle
			}
			if style != lastStyle {
				flush()
				lastStyle = style
			}
			run.WriteRune(rune(0x2800 + bits))
		}
		flush()
		b.WriteString(line.String())
		if row < hCells-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

