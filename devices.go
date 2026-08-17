package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// DefaultDevice is the sentinel for the system default input device.
const DefaultDevice = "System default"

var avDeviceRe = regexp.MustCompile(`\[(\d+)\]\s+(.+)$`)

// listInputDevices returns the coreaudio capture device names, using
// ffmpeg's avfoundation listing (the names match what sox -t coreaudio
// accepts). The system default is always first.
func listInputDevices() []string {
	devices := []string{DefaultDevice}
	ffmpeg, err := findBin("ffmpeg")
	if err != nil {
		return devices
	}
	// avfoundation lists devices on stderr and exits non-zero; that's normal.
	out, _ := exec.Command(ffmpeg, "-f", "avfoundation", "-list_devices", "true", "-i", "").CombinedOutput()
	inAudio := false
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "AVFoundation audio devices") {
			inAudio = true
			continue
		}
		if strings.Contains(line, "AVFoundation video devices") {
			inAudio = false
			continue
		}
		if !inAudio {
			continue
		}
		if m := avDeviceRe.FindStringSubmatch(line); m != nil {
			devices = append(devices, strings.TrimSpace(m[2]))
		}
	}
	return devices
}

// findBin resolves a binary from PATH plus the usual Homebrew and nix
// profile locations, so the TUI works even from a minimal environment.
func findBin(name string) (string, error) {
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	home, _ := os.UserHomeDir()
	for _, dir := range []string{
		"/opt/homebrew/bin", "/usr/local/bin", "/run/current-system/sw/bin",
		"/etc/profiles/per-user/" + os.Getenv("USER") + "/bin",
		home + "/.local/bin",
	} {
		p := dir + "/" + name
		if info, err := os.Stat(p); err == nil && info.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s not found on PATH", name)
}
