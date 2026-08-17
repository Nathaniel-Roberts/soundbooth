package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Config persists the last-used setup choices.
type Config struct {
	Device     string `json:"device"`
	OutDir     string `json:"out_dir"`
	Channels   int    `json:"channels"` // 1 = mono, 2 = stereo
	Mode       string `json:"mode"`     // "record" or "armed"
	BufferMin  int    `json:"buffer_min"`
	Transcribe bool   `json:"transcribe"`
	Model      string `json:"model"`
	Speakers   int    `json:"speakers"` // 0 = auto
	Language   string `json:"language"`

	// RetentionDays > 0 auto-deletes soundbooth recordings older than N
	// days on startup; transcripts and markers are kept. 0 = keep forever.
	RetentionDays int `json:"retention_days"`

	Theme       string            `json:"theme"`
	ThemeColors map[string]string `json:"theme_colors,omitempty"` // per-colour overrides
	// PostCommand runs via `sh -c` after a successful transcription with
	// SB_AUDIO, SB_TRANSCRIPT_DIR, SB_TRANSCRIPT_MD, SB_MARKERS in the env.
	PostCommand string `json:"post_command,omitempty"`
}

func defaultConfig() Config {
	home, _ := os.UserHomeDir()
	return Config{
		Device:     DefaultDevice,
		OutDir:     filepath.Join(home, "Recordings"),
		Channels:   1,
		Mode:       "record",
		BufferMin:  10,
		Transcribe: true,
		Model:      "large-v3-turbo",
		Speakers:   0,
		Language:   "en",
		Theme:      "mocha",
	}
}

func configPath() string {
	home, _ := os.UserHomeDir()
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "soundbooth", "config.json")
}

func loadConfig() Config {
	cfg := defaultConfig()
	data, err := os.ReadFile(configPath())
	if err != nil {
		return cfg
	}
	_ = json.Unmarshal(data, &cfg)
	if cfg.OutDir == "" {
		cfg.OutDir = defaultConfig().OutDir
	}
	if cfg.Model == "" {
		cfg.Model = defaultConfig().Model
	}
	if cfg.Device == "" {
		cfg.Device = DefaultDevice
	}
	if cfg.Language == "" {
		cfg.Language = "en"
	}
	if cfg.Channels != 1 && cfg.Channels != 2 {
		cfg.Channels = 1
	}
	if cfg.Mode != "armed" {
		cfg.Mode = "record"
	}
	if cfg.BufferMin < 0 || cfg.BufferMin > 60 {
		cfg.BufferMin = 10
	}
	if cfg.Theme == "" {
		cfg.Theme = "mocha"
	}
	if cfg.RetentionDays < 0 || cfg.RetentionDays > 3650 {
		cfg.RetentionDays = 0
	}
	return cfg
}

func (c Config) save() error {
	path := configPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// hfTokenPresent reports whether the gated pyannote diarisation model can
// be used (whispermlx reads the Hugging Face token from this file).
func hfTokenPresent() bool {
	home, _ := os.UserHomeDir()
	info, err := os.Stat(filepath.Join(home, ".cache", "huggingface", "token"))
	return err == nil && info.Size() > 0
}
