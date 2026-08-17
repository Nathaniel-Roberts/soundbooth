use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::path::PathBuf;

pub const DEFAULT_DEVICE: &str = "System default";

/// Persisted setup choices. Field names match the Go version's JSON so
/// existing configs carry straight over.
#[derive(Clone, Serialize, Deserialize)]
#[serde(default)]
pub struct Config {
    pub device: String,
    pub out_dir: String,
    pub channels: u16, // 1 = mono, 2 = stereo
    pub system_audio: bool,
    pub mode: String, // "record" or "armed"
    pub buffer_min: u32,
    pub transcribe: bool,
    pub live_captions: bool,
    pub model: String,
    pub speakers: u32, // 0 = auto
    pub language: String,
    pub retention_days: u32,
    pub theme: String,
    #[serde(skip_serializing_if = "HashMap::is_empty")]
    pub theme_colors: HashMap<String, String>,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub post_command: String,
}

impl Default for Config {
    fn default() -> Self {
        let home = dirs::home_dir().unwrap_or_else(|| PathBuf::from("."));
        Config {
            device: DEFAULT_DEVICE.into(),
            out_dir: home.join("Recordings").to_string_lossy().into_owned(),
            channels: 1,
            system_audio: false,
            mode: "record".into(),
            buffer_min: 10,
            transcribe: true,
            live_captions: false,
            model: "large-v3-turbo".into(),
            speakers: 0,
            language: "en".into(),
            retention_days: 0,
            theme: "mocha".into(),
            theme_colors: HashMap::new(),
            post_command: String::new(),
        }
    }
}

pub fn config_path() -> PathBuf {
    let base = std::env::var_os("XDG_CONFIG_HOME")
        .map(PathBuf::from)
        .unwrap_or_else(|| dirs::home_dir().unwrap_or_default().join(".config"));
    base.join("soundbooth").join("config.json")
}

pub fn load() -> Config {
    let mut cfg: Config = std::fs::read_to_string(config_path())
        .ok()
        .and_then(|s| serde_json::from_str(&s).ok())
        .unwrap_or_default();
    let d = Config::default();
    if cfg.out_dir.is_empty() {
        cfg.out_dir = d.out_dir;
    }
    if cfg.model.is_empty() {
        cfg.model = d.model;
    }
    if cfg.device.is_empty() {
        cfg.device = d.device;
    }
    if cfg.language.is_empty() {
        cfg.language = d.language;
    }
    if cfg.channels != 1 && cfg.channels != 2 {
        cfg.channels = 1;
    }
    if cfg.mode != "armed" {
        cfg.mode = "record".into();
    }
    if cfg.buffer_min > 60 {
        cfg.buffer_min = 10;
    }
    if cfg.theme.is_empty() {
        cfg.theme = "mocha".into();
    }
    if cfg.retention_days > 3650 {
        cfg.retention_days = 0;
    }
    cfg
}

impl Config {
    pub fn save(&self) -> std::io::Result<()> {
        let path = config_path();
        if let Some(dir) = path.parent() {
            std::fs::create_dir_all(dir)?;
        }
        std::fs::write(path, serde_json::to_string_pretty(self).unwrap_or_default())
    }
}

/// The gated pyannote model needs a Hugging Face token here.
pub fn hf_token_path() -> PathBuf {
    dirs::home_dir().unwrap_or_default().join(".cache/huggingface/token")
}

pub fn hf_token_present() -> bool {
    std::fs::metadata(hf_token_path()).map(|m| m.len() > 0).unwrap_or(false)
}
