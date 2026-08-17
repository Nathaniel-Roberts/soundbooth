package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Transcriber streams whispermlx output while it runs.
type Transcriber struct {
	Lines chan string
	Done  chan error

	OutDir string
	cmd    *exec.Cmd
}

func startTranscribe(file, model, language string, speakers int) (*Transcriber, error) {
	wmlx, err := findBin("whispermlx")
	if err != nil {
		return nil, fmt.Errorf("whispermlx not found — install with: uv tool install --python 3.13 --with 'numba>=0.61' whispermlx")
	}
	home, _ := os.UserHomeDir()
	tokenPath := filepath.Join(home, ".cache", "huggingface", "token")
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("no Hugging Face token at %s (needed for pyannote diarisation)", tokenPath)
	}

	base := filepath.Base(file)
	outDir := filepath.Join(filepath.Dir(file), strings.TrimSuffix(base, filepath.Ext(base)))
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}

	args := []string{
		file,
		"--model", model,
		"--diarize",
		"--hf_token", strings.TrimSpace(string(token)),
		"--output_format", "all",
		"--output_dir", outDir,
	}
	if language != "" && language != "auto" {
		args = append(args, "--language", language)
	}
	if speakers > 0 {
		s := strconv.Itoa(speakers)
		args = append(args, "--min_speakers", s, "--max_speakers", s)
	}

	cmd := exec.Command(wmlx, args...)
	// Silence the harmless torchcodec/numpy warning wall (same fix as the
	// shell wrapper): pyannote falls back to its own audio loader.
	cmd.Env = append(os.Environ(), "PYTHONWARNINGS=ignore")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = cmd.Stdout // interleave; whispermlx logs to stderr

	t := &Transcriber{
		Lines:  make(chan string, 256),
		Done:   make(chan error, 1),
		OutDir: outDir,
		cmd:    cmd,
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			select {
			case t.Lines <- line:
			default:
			}
		}
		close(t.Lines)
		t.Done <- cmd.Wait()
	}()
	return t, nil
}
