package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// waveCol is one rendered sub-column (one meter tick = one Braille dot
// column, i.e. 50 ms per dot column, 100 ms per character cell).
type waveCol struct {
	rms, peak   float64 // left (or mono)
	rmsR, peakR float64 // right
	clip        bool
	paused      bool
}

// --- colour helpers ---

func hexRGB(hex string) (int, int, int) {
	hex = strings.TrimPrefix(hex, "#")
	if len(hex) != 6 {
		return 205, 214, 244
	}
	r, _ := strconv.ParseInt(hex[0:2], 16, 32)
	g, _ := strconv.ParseInt(hex[2:4], 16, 32)
	b, _ := strconv.ParseInt(hex[4:6], 16, 32)
	return int(r), int(g), int(b)
}

func mixHex(a, b string, t float64) string {
	if t < 0 {
		t = 0
	}
	if t > 1 {
		t = 1
	}
	ar, ag, ab := hexRGB(a)
	br, bg, bb := hexRGB(b)
	return fmt.Sprintf("#%02x%02x%02x",
		int(float64(ar)+(float64(br-ar))*t),
		int(float64(ag)+(float64(bg-ag))*t),
		int(float64(ab)+(float64(bb-ab))*t))
}

// waveRamp is the vertical gradient: lavender at the centre, through blue,
// to sapphire at the tips.
func waveRamp(t float64) string {
	if t < 0.55 {
		return mixHex(th.Lavender, th.Blue, t/0.55)
	}
	return mixHex(th.Blue, th.Sapphire, (t-0.55)/0.45)
}

// vuRamp colours the level bar green -> yellow -> red across its width.
func vuRamp(t float64) string {
	if t < 0.76 {
		return mixHex(th.Green, th.Yellow, t/0.76)
	}
	return mixHex(th.Yellow, th.Red, (t-0.76)/0.24)
}

// brailleBits is the bit for dot (x 0..1, y 0..3) inside a Braille cell.
var brailleBits = [2][4]int{
	{0x01, 0x02, 0x04, 0x40},
	{0x08, 0x10, 0x20, 0x80},
}

// renderWaveStereo draws two stacked lanes (L over R) within hCells rows.
func renderWaveStereo(cols []waveCol, wCells, hCells int) string {
	lane := hCells / 2
	if lane < 3 {
		return renderWave(cols, wCells, hCells)
	}
	right := make([]waveCol, len(cols))
	for i, c := range cols {
		right[i] = waveCol{rms: c.rmsR, peak: c.peakR, clip: c.clip, paused: c.paused}
	}
	top := renderWave(cols, wCells, lane)
	bottom := renderWave(right, wCells, lane)
	return top + "\n" + bottom
}

// renderWave draws the last cols of audio as an Audacity-style waveform:
// mirrored around a centre line, Braille dots for 2x4 sub-cell resolution,
// a vertical Catppuccin gradient (lavender core to sapphire tips, RMS
// cells brightened), red for clipped columns, grey for paused spans and
// the idle hairline.
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
	paused := make([]bool, need)
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
		paused[pad+i] = c.paused
	}
	for i := 0; i < pad; i++ {
		env[i] = 1
	}

	clipHex := th.Red
	greyHex := th.Overlay0

	var b strings.Builder
	for row := 0; row < hCells; row++ {
		// Row gradient position: outermost dot's distance from centre.
		outer := 0
		for dy := 0; dy < 4; dy++ {
			y := row*4 + dy
			var dist int
			if y < half {
				dist = half - 1 - y
			} else {
				dist = y - half
			}
			if dist+1 > outer {
				outer = dist + 1
			}
		}
		t := float64(outer) / float64(half)
		envHex := waveRamp(t)
		coreHex := mixHex(envHex, th.Text, 0.45)

		var line strings.Builder
		lastHex := ""
		var run strings.Builder
		flush := func() {
			if run.Len() > 0 {
				if lastHex == "" {
					line.WriteString(run.String())
				} else {
					line.WriteString(lipgloss.NewStyle().
						Foreground(lipgloss.Color(lastHex)).Render(run.String()))
				}
				run.Reset()
			}
		}
		emit := func(hex string, r rune) {
			if hex != lastHex {
				flush()
				lastHex = hex
			}
			run.WriteRune(r)
		}

		for cx := 0; cx < wCells; cx++ {
			bits := 0
			cellCore := true // whole cell inside the RMS core?
			cellClip := false
			cellPaused := false
			cellMaxEnv := 0
			for sub := 0; sub < 2; sub++ {
				ci := cx*2 + sub
				if clip[ci] {
					cellClip = true
				}
				if paused[ci] {
					cellPaused = true
				}
				if env[ci] > cellMaxEnv {
					cellMaxEnv = env[ci]
				}
				for dy := 0; dy < 4; dy++ {
					y := row*4 + dy
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
			if bits == 0 {
				emit(lastHex, ' ')
				continue
			}
			hex := envHex
			switch {
			case cellClip:
				hex = clipHex
			case cellPaused, cellMaxEnv <= 1: // paused spans and the idle hairline
				hex = greyHex
			case cellCore:
				hex = coreHex
			}
			emit(hex, rune(0x2800+bits))
		}
		flush()
		b.WriteString(line.String())
		if row < hCells-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// renderRuler draws a DAW-style time ruler under the waveform. cellMs is
// the duration of one character cell; marks land every 5/15/30 s depending
// on zoom. markerCells are cells-back-from-the-right-edge where the user
// dropped markers.
func renderRuler(wCells, cellMs int, markerCells []int) string {
	if cellMs <= 0 {
		cellMs = 100
	}
	stepSec := 5
	if cellMs >= 200 {
		stepSec = 15
	}
	stepCells := stepSec * 1000 / cellMs
	if stepCells < 1 {
		stepCells = 1
	}
	marks := make([]rune, wCells)
	for i := range marks {
		marks[i] = '╌'
	}
	labels := make([]rune, wCells)
	for i := range labels {
		labels[i] = ' '
	}
	place := func(pos int, s string) {
		if len(s) > wCells {
			return
		}
		start := pos - len(s)/2
		if start < 0 {
			start = 0
		}
		if start+len(s) > wCells {
			start = wCells - len(s)
		}
		for i, r := range s {
			labels[start+i] = r
		}
	}
	for k := 0; ; k++ {
		pos := wCells - 1 - k*stepCells
		if pos < 0 {
			break
		}
		marks[pos] = '┴'
		if k == 0 {
			place(pos, "now")
		} else {
			place(pos, fmt.Sprintf("-%ds", k*stepSec))
		}
	}
	line := dimStyle.Render(string(marks))
	// overlay marker arrows in mauve
	if len(markerCells) > 0 {
		markRunes := []rune(string(marks))
		overlay := make([]bool, wCells)
		for _, back := range markerCells {
			pos := wCells - 1 - back
			if pos >= 0 && pos < wCells {
				markRunes[pos] = '▼'
				overlay[pos] = true
			}
		}
		var b strings.Builder
		for i, r := range markRunes {
			if overlay[i] {
				b.WriteString(lipgloss.NewStyle().
					Foreground(lipgloss.Color(th.Mauve)).Render(string(r)))
			} else {
				b.WriteString(dimStyle.Render(string(r)))
			}
		}
		line = b.String()
	}
	return line + "\n" + dimStyle.Render(string(labels))
}

// downsample max-pools groups of z ticks into one sub-column, so zoomed-out
// views keep peaks visible.
func downsample(cols []waveCol, z int) []waveCol {
	if z <= 1 {
		return cols
	}
	out := make([]waveCol, 0, len(cols)/z+1)
	// group from the tail so the newest tick is always in the last group
	start := len(cols) % z
	if start > 0 {
		out = append(out, poolCols(cols[:start]))
	}
	for i := start; i+z <= len(cols); i += z {
		out = append(out, poolCols(cols[i:i+z]))
	}
	return out
}

func poolCols(group []waveCol) waveCol {
	var c waveCol
	for _, g := range group {
		if g.peak > c.peak {
			c.peak = g.peak
		}
		if g.rms > c.rms {
			c.rms = g.rms
		}
		if g.peakR > c.peakR {
			c.peakR = g.peakR
		}
		if g.rmsR > c.rmsR {
			c.rmsR = g.rmsR
		}
		c.clip = c.clip || g.clip
		c.paused = c.paused || g.paused
	}
	return c
}

// renderVU draws a gradient level bar with a peak-hold marker.
// level and hold are 0..1 on the dB scale.
func renderVU(width int, level, hold float64) string {
	if width < 10 {
		width = 10
	}
	fill := int(level*float64(width) + 0.5)
	holdPos := int(hold * float64(width))
	if holdPos >= width {
		holdPos = width - 1
	}
	var b strings.Builder
	for i := 0; i < width; i++ {
		t := float64(i) / float64(width-1)
		switch {
		case i == holdPos && hold > 0.01:
			b.WriteString(lipgloss.NewStyle().
				Foreground(lipgloss.Color(th.Text)).Render("▐"))
		case i < fill:
			b.WriteString(lipgloss.NewStyle().
				Foreground(lipgloss.Color(vuRamp(t))).Render("█"))
		default:
			b.WriteString(lipgloss.NewStyle().
				Foreground(lipgloss.Color(th.Surface0)).Render("░"))
		}
	}
	db := dbFloor * (1 - hold)
	return b.String() + dimStyle.Render(fmt.Sprintf(" %4.0f dB", db))
}
