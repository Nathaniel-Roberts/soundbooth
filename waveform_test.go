package main

import (
	"strings"
	"testing"
)

func TestRenderWaveDimensions(t *testing.T) {
	cols := make([]waveCol, 40)
	for i := range cols {
		cols[i] = waveCol{rms: 0.4, peak: 0.8}
	}
	out := renderWave(cols, 30, 8)
	lines := strings.Split(out, "\n")
	if len(lines) != 8 {
		t.Fatalf("expected 8 rows, got %d", len(lines))
	}
	if !strings.ContainsRune(out, '⣿') {
		t.Errorf("expected full braille cells for a loud signal")
	}
}

func TestRenderWaveSilenceHairline(t *testing.T) {
	cols := make([]waveCol, 20)
	out := renderWave(cols, 10, 5)
	// silence must still draw something (the centre hairline), but no full cells
	if !strings.ContainsAny(out, "⠀⣿") && len(out) == 0 {
		t.Fatal("empty render for silence")
	}
	if strings.ContainsRune(out, '⣿') {
		t.Errorf("silence should not render full cells")
	}
}

func TestRenderWaveStereoLanes(t *testing.T) {
	cols := make([]waveCol, 20)
	for i := range cols {
		cols[i] = waveCol{rms: 0.3, peak: 0.7, rmsR: 0.1, peakR: 0.2}
	}
	out := renderWaveStereo(cols, 10, 8)
	lines := strings.Split(out, "\n")
	if len(lines) != 8 {
		t.Fatalf("expected 8 rows (two 4-row lanes), got %d", len(lines))
	}
}

func TestRulerAndVUBounds(t *testing.T) {
	for _, w := range []int{1, 2, 5, 40, 200} {
		for _, cellMs := range []int{0, 100, 200, 500} {
			_ = renderRuler(w, cellMs, nil) // must not panic at any width
			_ = renderRuler(w, cellMs, []int{0, 3, w - 1, w + 10, -2})
		}
	}
	for _, lvl := range []float64{-0.5, 0, 0.5, 1, 1.5} {
		_ = renderVU(5, lvl, lvl)
		_ = renderVU(60, lvl, 1)
	}
}

func TestDownsample(t *testing.T) {
	cols := make([]waveCol, 10)
	for i := range cols {
		cols[i].peak = float64(i) / 10
	}
	cols[9].clip = true
	out := downsample(cols, 5)
	if len(out) != 2 {
		t.Fatalf("expected 2 pooled columns, got %d", len(out))
	}
	if out[1].peak != 0.9 {
		t.Errorf("max-pool lost the peak: %v", out[1].peak)
	}
	if !out[1].clip {
		t.Error("max-pool lost the clip flag")
	}
	if got := downsample(cols, 1); len(got) != 10 {
		t.Errorf("z=1 should be identity")
	}
}

func TestWaveGradientColours(t *testing.T) {
	if got := mixHex("#000000", "#ffffff", 0.5); got != "#7f7f7f" {
		t.Errorf("mixHex midpoint = %s", got)
	}
	if waveRamp(0) != "#b4befe" {
		t.Errorf("ramp(0) = %s, want lavender", waveRamp(0))
	}
	if waveRamp(1) != "#74c7ec" {
		t.Errorf("ramp(1) = %s, want sapphire", waveRamp(1))
	}
}

func TestLvlMapping(t *testing.T) {
	if lvl(0) != 0 {
		t.Errorf("lvl(0) = %v, want 0", lvl(0))
	}
	if got := lvl(1.0); got != 1.0 {
		t.Errorf("lvl(1.0) = %v, want 1.0 (0 dBFS)", got)
	}
	// -50 dBFS floor -> 0
	if got := lvl(0.00316); got > 0.01 {
		t.Errorf("lvl at -50 dB = %v, want ~0", got)
	}
}
