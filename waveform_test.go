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
