package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "trigger", "stop", "marker":
			if err := sendControl(os.Args[1]); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
			fmt.Println("ok")
			return
		case "doctor":
			os.Exit(runDoctor())
		case "devices":
			for _, d := range listInputDevices() {
				fmt.Println(d.Name)
			}
			return
		}
	}

	listDevices := flag.Bool("devices", false, "list input devices and exit")
	flag.Parse()
	if *listDevices {
		for _, d := range listInputDevices() {
			fmt.Println(d.Name)
		}
		return
	}

	p := tea.NewProgram(newModel(), tea.WithAltScreen())
	ln, err := startControl(p)
	if err == nil {
		defer func() {
			_ = ln.Close()
			_ = os.Remove(controlSockPath())
		}()
	}
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// runDoctor checks the toolchain and reports what is missing.
func runDoctor() int {
	fails := 0
	check := func(label string, ok bool, fix string) {
		mark := "ok "
		if !ok {
			mark = "FAIL"
			fails++
		}
		fmt.Printf("  [%s] %s", mark, label)
		if !ok && fix != "" {
			fmt.Printf(" — %s", fix)
		}
		fmt.Println()
	}
	has := func(bin string) bool { _, err := findBin(bin); return err == nil }

	fmt.Println("soundbooth doctor")
	check("sox (capture + metering)", has("sox"), "brew install sox")
	check("ffmpeg (device listing + armed-mode buffer)", has("ffmpeg"), "brew install ffmpeg")
	check("whispermlx (transcription)", has("whispermlx"),
		"uv tool install --python 3.13 --with 'numba>=0.61' whispermlx")
	check("afplay (playback)", has("afplay"), "")
	check("Hugging Face token (diarisation)", hfTokenPresent(),
		"accept pyannote terms, save a read token to ~/.cache/huggingface/token")

	home, _ := os.UserHomeDir()
	free := freeBytes(filepath.Join(home, "Recordings"))
	if free < 0 {
		free = freeBytes(home)
	}
	check(fmt.Sprintf("disk space (%s free)", humanSize(free)), free > 2<<30, "free up some space")

	devices := listInputDevices()
	check(fmt.Sprintf("input devices (%d found)", len(devices)-1), len(devices) > 1,
		"check microphone permissions in System Settings")

	if fails == 0 {
		fmt.Println("all good")
		return 0
	}
	fmt.Printf("%d problem(s)\n", fails)
	return 1
}
