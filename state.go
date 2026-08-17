package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// stateDir holds crash-recoverable session segments and the control socket.
func stateDir() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "soundbooth")
}

func sessionsDir() string { return filepath.Join(stateDir(), "sessions") }

// sessionMeta records enough about an in-flight recording to finish it
// after a crash.
type sessionMeta struct {
	File     string    `json:"file"`
	Name     string    `json:"name"`
	Channels int       `json:"channels"`
	Started  time.Time `json:"started"`
}

func writeMeta(dir string, m sessionMeta) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "meta.json"), data, 0o644)
}

func readMeta(dir string) (sessionMeta, error) {
	var m sessionMeta
	data, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return m, err
	}
	err = json.Unmarshal(data, &m)
	return m, err
}

// orphan is an unfinished session left behind by a crash.
type orphan struct {
	Dir      string
	Meta     sessionMeta
	Segments []string
	Bytes    int64
}

// EstDuration guesses the recording length from FLAC size (~50 KB/s for
// mono speech).
func (o orphan) EstDuration() time.Duration {
	perSec := int64(50 * 1024 * o.Meta.Channels)
	if perSec == 0 {
		perSec = 50 * 1024
	}
	return time.Duration(o.Bytes/perSec) * time.Second
}

// findOrphans scans the sessions dir for directories with audio in them.
// Empty leftovers are removed on sight.
func findOrphans() []orphan {
	entries, err := os.ReadDir(sessionsDir())
	if err != nil {
		return nil
	}
	var out []orphan
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(sessionsDir(), e.Name())
		o := orphan{Dir: dir}
		o.Meta, _ = readMeta(dir)
		segs, _ := filepath.Glob(filepath.Join(dir, "seg-*.flac"))
		for _, s := range segs {
			if info, err := os.Stat(s); err == nil && info.Size() > 0 {
				o.Segments = append(o.Segments, s)
				o.Bytes += info.Size()
			}
		}
		if len(o.Segments) == 0 {
			_ = os.RemoveAll(dir)
			continue
		}
		sort.Strings(o.Segments)
		out = append(out, o)
	}
	return out
}

// sweepStaleSpools removes leftover armed-mode spool dirs from previous
// runs — the replay buffer must never survive a crash.
func sweepStaleSpools() {
	matches, _ := filepath.Glob(filepath.Join(os.TempDir(), "soundbooth-spool-*"))
	for _, m := range matches {
		if strings.Contains(m, "soundbooth-spool-") {
			_ = os.RemoveAll(m)
		}
	}
}

// recNameRe matches soundbooth's own recording filenames — retention must
// never touch audio the user put in the folder themselves.
var recNameRe = regexp.MustCompile(`-\d{8}-\d{6}\.flac$`)

// sweepRetention deletes soundbooth recordings older than days. Transcript
// directories and marker files are kept — only the audio goes.
func sweepRetention(dir string, days int) int {
	if days <= 0 {
		return 0
	}
	cutoff := time.Now().AddDate(0, 0, -days)
	matches, _ := filepath.Glob(filepath.Join(dir, "*.flac"))
	n := 0
	for _, p := range matches {
		if !recNameRe.MatchString(p) {
			continue
		}
		info, err := os.Stat(p)
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if os.Remove(p) == nil {
			n++
		}
	}
	return n
}

// freeBytes reports available disk space at path.
func freeBytes(path string) int64 {
	var stat statfsT
	if err := statfs(path, &stat); err != nil {
		return -1
	}
	return int64(stat.Bavail) * int64(stat.Bsize)
}
