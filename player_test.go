package main

import (
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestDecodeWave(t *testing.T) {
	soxPath, err := findBin("sox")
	if err != nil {
		t.Skip("sox not installed")
	}
	f := filepath.Join(t.TempDir(), "tone.flac")
	if out, err := exec.Command(soxPath, "-n", "-r", "48000", "-c", "1", f,
		"synth", "3", "sine", "440", "vol", "0.5").CombinedOutput(); err != nil {
		t.Fatalf("synth: %v: %s", err, out)
	}
	msg, ok := decodeWave(f, 120)().(waveReadyMsg)
	if !ok || msg.err != nil {
		t.Fatalf("decode failed: %+v", msg)
	}
	if msg.dur < 2.9 || msg.dur > 3.1 {
		t.Errorf("duration = %v, want ~3s", msg.dur)
	}
	if len(msg.cols) < 100 || len(msg.cols) > 120 {
		t.Errorf("cols = %d, want ~120", len(msg.cols))
	}
	loud := 0
	for _, c := range msg.cols {
		if c.peak > 0.5 {
			loud++
		}
	}
	if loud < len(msg.cols)/2 {
		t.Errorf("sine tone should be loud in most columns, got %d/%d", loud, len(msg.cols))
	}
}

func TestMarkersRoundtrip(t *testing.T) {
	audio := filepath.Join(t.TempDir(), "meet-20260817-000000.flac")
	in := []time.Duration{65 * time.Second, 3725 * time.Second}
	path := writeMarkersFile(audio, in)
	if path == "" {
		t.Fatal("markers file not written")
	}
	out := loadMarkersFromFile(audio)
	if len(out) != 2 || out[0] != in[0] || out[1] != in[1] {
		t.Fatalf("roundtrip mismatch: %v -> %v", in, out)
	}
}
