package main

import (
	"fmt"
	"strconv"
	"strings"
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

// --- hot-path colour caches ---
// The live view repaints up to 40x/second; building lipgloss styles per
// colour run allocates heavily and made the UI stutter. The renderers
// below write raw truecolor escapes from these caches instead (safe: the
// program forces the truecolor profile at startup).

const ansiReset = "\x1b[0m"

var fgCache = map[string]string{}

func fgEsc(hex string) string {
	if e, ok := fgCache[hex]; ok {
		return e
	}
	r, g, b := hexRGB(hex)
	e := fmt.Sprintf("\x1b[38;2;%d;%d;%dm", r, g, b)
	fgCache[hex] = e
	return e
}

type rampKey struct{ gen, half, outer int }

var rampCache = map[rampKey][2]string{}

// rowEscs returns the cached envelope and core escapes for a wave row.
func rowEscs(half, outer int) (string, string) {
	k := rampKey{renderGen, half, outer}
	if v, ok := rampCache[k]; ok {
		return v[0], v[1]
	}
	t := float64(outer) / float64(half)
	env := waveRamp(t)
	v := [2]string{fgEsc(env), fgEsc(mixHex(env, th.Text, 0.45))}
	rampCache[k] = v
	return v[0], v[1]
}

type barKey struct{ gen, width int }

var vuColourCache = map[barKey][]string{}

func vuColours(width int) []string {
	k := barKey{renderGen, width}
	if v, ok := vuColourCache[k]; ok {
		return v
	}
	v := make([]string, width)
	for i := range v {
		v[i] = fgEsc(vuRamp(float64(i) / float64(width-1)))
	}
	vuColourCache[k] = v
	return v
}

var progColourCache = map[barKey][]string{}

func progColours(width int) []string {
	k := barKey{renderGen, width}
	if v, ok := progColourCache[k]; ok {
		return v
	}
	v := make([]string, width)
	for i := range v {
		v[i] = fgEsc(waveRamp(float64(i) / float64(width-1)))
	}
	progColourCache[k] = v
	return v
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

// renderWave draws with no playhead (live view, previews).
func renderWave(cols []waveCol, wCells, hCells int) string {
	return renderWaveHead(cols, wCells, hCells, -1)
}

// renderWaveHead draws the last cols of audio as an Audacity-style
// waveform: mirrored around a centre line, Braille dots for 2x4 sub-cell
// resolution, a vertical Catppuccin gradient (lavender core to sapphire
// tips, RMS cells brightened), red for clipped columns, grey for paused
// spans and the idle hairline. playhead >= 0 highlights that cell column
// (the seekable player's cursor).
func renderWaveHead(cols []waveCol, wCells, hCells int, playhead int) string {
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

	clipEsc := fgEsc(th.Red)
	greyEsc := fgEsc(th.Overlay0)
	headEsc := fgEsc(th.Text)

	var b strings.Builder
	b.Grow(hCells * wCells * 4)
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
		envEsc, coreEsc := rowEscs(half, outer)

		lastEsc := ""
		emit := func(esc string, r rune) {
			// spaces render identically in any colour: never switch for them
			if r != ' ' && esc != lastEsc {
				b.WriteString(esc)
				lastEsc = esc
			}
			b.WriteRune(r)
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
			if cx == playhead {
				// player cursor: a bright full-height dot column
				c := rune(0x2800 + (bits | 0x01 | 0x02 | 0x04 | 0x40))
				emit(headEsc, c)
				continue
			}
			if bits == 0 {
				emit(lastEsc, ' ')
				continue
			}
			esc := envEsc
			switch {
			case cellClip:
				esc = clipEsc
			case cellPaused, cellMaxEnv <= 1: // paused spans and the idle hairline
				esc = greyEsc
			case cellCore:
				esc = coreEsc
			}
			emit(esc, rune(0x2800+bits))
		}
		b.WriteString(ansiReset)
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
		overlay := make([]bool, wCells)
		for _, back := range markerCells {
			pos := wCells - 1 - back
			if pos >= 0 && pos < wCells {
				marks[pos] = '▼'
				overlay[pos] = true
			}
		}
		dimEsc := fgEsc(th.Overlay0)
		mauveEsc := fgEsc(th.Mauve)
		var b strings.Builder
		lastEsc := ""
		for i, r := range marks {
			esc := dimEsc
			if overlay[i] {
				esc = mauveEsc
			}
			if esc != lastEsc {
				b.WriteString(esc)
				lastEsc = esc
			}
			b.WriteRune(r)
		}
		b.WriteString(ansiReset)
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
	colours := vuColours(width)
	emptyEsc := fgEsc(th.Surface0)
	textEsc := fgEsc(th.Text)
	var b strings.Builder
	b.Grow(width * 16)
	lastEsc := ""
	for i := 0; i < width; i++ {
		var esc string
		var glyph string
		switch {
		case i == holdPos && hold > 0.01:
			esc, glyph = textEsc, "▐"
		case i < fill:
			esc, glyph = colours[i], "█"
		default:
			esc, glyph = emptyEsc, "░"
		}
		if esc != lastEsc {
			b.WriteString(esc)
			lastEsc = esc
		}
		b.WriteString(glyph)
	}
	b.WriteString(ansiReset)
	db := dbFloor * (1 - hold)
	return b.String() + dimStyle.Render(fmt.Sprintf(" %4.0f dB", db))
}
