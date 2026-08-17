package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSweepRetention(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "meeting-20250101-090000.flac")
	fresh := filepath.Join(dir, "meeting-20991231-090000.flac")
	foreign := filepath.Join(dir, "holiday-song.flac") // not ours: never delete
	tx := filepath.Join(dir, "meeting-20250101-090000")
	for _, f := range []string{old, fresh, foreign} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.MkdirAll(tx, 0o755)
	past := time.Now().AddDate(0, 0, -40)
	_ = os.Chtimes(old, past, past)
	_ = os.Chtimes(foreign, past, past)

	if n := sweepRetention(dir, 30); n != 1 {
		t.Fatalf("expected 1 deletion, got %d", n)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("old recording should be deleted")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("fresh recording should survive")
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Error("non-soundbooth flac must never be touched")
	}
	if _, err := os.Stat(tx); err != nil {
		t.Error("transcript dir must be kept")
	}
	if n := sweepRetention(dir, 0); n != 0 {
		t.Error("retention off must delete nothing")
	}
}

func TestSearchTranscripts(t *testing.T) {
	dir := t.TempDir()
	audio := filepath.Join(dir, "standup-20260817-090000.flac")
	txDir := filepath.Join(dir, "standup-20260817-090000")
	_ = os.WriteFile(audio, []byte("x"), 0o644)
	_ = os.MkdirAll(txDir, 0o755)
	txt := "Scott talked about the kiosk rollout.\nBen covered the SCEP profiles.\nNathaniel raised the Palo rulebase.\n"
	_ = os.WriteFile(filepath.Join(txDir, "standup-20260817-090000.txt"), []byte(txt), 0o644)

	entries := loadLibrary(dir)
	hits := searchTranscripts(entries, "scep")
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(hits))
	}
	if hits[0].LineNo != 2 {
		t.Errorf("hit line = %d, want 2", hits[0].LineNo)
	}
	ctx := hitContext(hits[0], 1)
	if len(ctx) != 3 {
		t.Errorf("context lines = %d, want 3", len(ctx))
	}
	if hits := searchTranscripts(entries, "zzz-nope"); len(hits) != 0 {
		t.Errorf("expected no hits")
	}
}
