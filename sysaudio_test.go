package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestSystapCompiles(t *testing.T) {
	if _, err := exec.LookPath("xcrun"); err != nil {
		t.Skip("no xcrun")
	}
	bin, err := systapBinary()
	if err != nil {
		t.Fatalf("helper build failed: %v", err)
	}
	if info, err := os.Stat(bin); err != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("helper missing or not executable: %v", err)
	}
	// second call must hit the cache (same path, no rebuild errors)
	bin2, err := systapBinary()
	if err != nil || bin2 != bin {
		t.Fatalf("cache miss: %v %s", err, bin2)
	}
}

// TestSysCaptureLifecycle runs the real tap. Without Screen Recording
// permission it must fail fast via WaitReady (the graceful-fallback path);
// with permission it must produce a FLAC.
func TestSysCaptureLifecycle(t *testing.T) {
	hwGate(t)
	if _, err := exec.LookPath("xcrun"); err != nil {
		t.Skip("no xcrun")
	}
	file := filepath.Join(t.TempDir(), "sys.flac")
	s, err := startSysCapture(file)
	if err != nil {
		t.Fatalf("setup error: %v", err)
	}
	defer s.Stop()
	if err := s.WaitReady(6 * time.Second); err != nil {
		t.Logf("tap not permitted here (expected without Screen Recording grant): %v", err)
		return // fallback path exercised
	}
	time.Sleep(2 * time.Second)
	s.Stop()
	info, err := os.Stat(file)
	if err != nil || info.Size() == 0 {
		t.Fatalf("system track missing or empty: %v", err)
	}
	t.Logf("system track: %d bytes", info.Size())
}

func TestMergeMicSystem(t *testing.T) {
	soxPath, err := findBin("sox")
	if err != nil {
		t.Skip("sox not installed")
	}
	dir := t.TempDir()
	mic := filepath.Join(dir, "mic.flac")
	sys := filepath.Join(dir, "sys.flac")
	out := filepath.Join(dir, "out.flac")
	for _, f := range [][2]string{{mic, "440"}, {sys, "880"}} {
		if b, err := exec.Command(soxPath, "-n", "-r", "48000", "-c", "1", f[0],
			"synth", "2", "sine", f[1], "vol", "0.5").CombinedOutput(); err != nil {
			t.Fatalf("synth: %v: %s", err, b)
		}
	}
	if err := mergeMicSystem(mic, sys, out); err != nil {
		t.Fatal(err)
	}
	soxi, _ := findBin("soxi")
	ch, _ := exec.Command(soxi, "-c", out).Output()
	if string(ch) != "2\n" {
		t.Errorf("merged channels = %q, want 2", string(ch))
	}
}
