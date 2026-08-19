use crossbeam_channel::{bounded, Receiver};
use std::io::{BufRead, BufReader};
use std::process::{Command, Stdio};

use crate::config::{hf_token_path, hf_token_present};
use crate::state::find_bin;

/// Everything soundbooth needs, checkable and (mostly) installable from
/// inside the app.
#[derive(Clone, Copy, PartialEq, Eq)]
pub enum ReqKey {
    Mic,
    Sox,
    Ffmpeg,
    Uv,
    Whispermlx,
    HfToken,
    Xcrun,
}

pub const REQ_ORDER: [ReqKey; 7] = [
    ReqKey::Mic,
    ReqKey::Sox,
    ReqKey::Ffmpeg,
    ReqKey::Uv,
    ReqKey::Whispermlx,
    ReqKey::HfToken,
    ReqKey::Xcrun,
];

#[derive(Clone, Copy, PartialEq)]
pub enum ReqStatus {
    Checking,
    Ok,
    Missing,
}

#[derive(Clone)]
pub struct Req {
    pub key: ReqKey,
    pub status: ReqStatus,
    pub detail: String,
}

impl ReqKey {
    pub fn label(self) -> &'static str {
        match self {
            ReqKey::Mic => "microphone",
            ReqKey::Sox => "sox (encode + playback)",
            ReqKey::Ffmpeg => "ffmpeg (armed-mode buffer)",
            ReqKey::Uv => "uv (python tool manager)",
            ReqKey::Whispermlx => "whispermlx (transcription)",
            ReqKey::HfToken => "Hugging Face token (diarisation)",
            ReqKey::Xcrun => "Xcode CLT (system audio, optional)",
        }
    }

    /// What pressing enter on a missing item does.
    pub fn action_hint(self) -> &'static str {
        match self {
            ReqKey::Mic => "check microphone permissions in System Settings",
            ReqKey::Sox | ReqKey::Ffmpeg | ReqKey::Uv => "enter installs via Homebrew",
            ReqKey::Whispermlx => "enter installs via uv (first transcription later downloads the ~1.6 GB model)",
            ReqKey::HfToken => "enter opens the two pages; then run the command shown in another terminal",
            ReqKey::Xcrun => "enter launches the Xcode command line tools installer",
        }
    }
}

// --- simulation hook for demos/tests -------------------------------------
// SOUNDBOOTH_FAKE_MISSING=whispermlx,uv makes those items report missing;
// their guided "install" is a short sleep, after which the fake clears and
// the real check takes over. Lets the wizard be exercised end to end on a
// fully set-up machine.

fn fake_name(key: ReqKey) -> &'static str {
    match key {
        ReqKey::Mic => "mic",
        ReqKey::Sox => "sox",
        ReqKey::Ffmpeg => "ffmpeg",
        ReqKey::Uv => "uv",
        ReqKey::Whispermlx => "whispermlx",
        ReqKey::HfToken => "hftoken",
        ReqKey::Xcrun => "xcrun",
    }
}

fn fakes() -> &'static std::sync::Mutex<std::collections::HashSet<String>> {
    static FAKES: std::sync::OnceLock<std::sync::Mutex<std::collections::HashSet<String>>> =
        std::sync::OnceLock::new();
    FAKES.get_or_init(|| {
        let set = std::env::var("SOUNDBOOTH_FAKE_MISSING")
            .unwrap_or_default()
            .split(',')
            .map(|s| s.trim().to_lowercase())
            .filter(|s| !s.is_empty())
            .collect();
        std::sync::Mutex::new(set)
    })
}

pub fn is_faked(key: ReqKey) -> bool {
    fakes().lock().unwrap().contains(fake_name(key))
}

pub fn clear_fake(key: ReqKey) {
    fakes().lock().unwrap().remove(fake_name(key));
}

pub fn check(key: ReqKey) -> (ReqStatus, String) {
    if is_faked(key) {
        return (ReqStatus::Missing, "(simulated for testing)".into());
    }
    let found = |p: Option<std::path::PathBuf>| match p {
        Some(p) => (ReqStatus::Ok, p.display().to_string()),
        None => (ReqStatus::Missing, String::new()),
    };
    match key {
        ReqKey::Mic => {
            let n = crate::audio::list_input_devices().len().saturating_sub(1);
            if n > 0 {
                (ReqStatus::Ok, format!("{n} device(s)"))
            } else {
                (ReqStatus::Missing, "no input devices visible".into())
            }
        }
        ReqKey::Sox => found(find_bin("sox")),
        ReqKey::Ffmpeg => found(find_bin("ffmpeg")),
        ReqKey::Uv => found(find_bin("uv")),
        ReqKey::Whispermlx => found(find_bin("whispermlx")),
        ReqKey::HfToken => {
            if hf_token_present() {
                (ReqStatus::Ok, hf_token_path().display().to_string())
            } else {
                (ReqStatus::Missing, String::new())
            }
        }
        ReqKey::Xcrun => found(find_bin("xcrun")),
    }
}

pub fn all_reqs() -> Vec<Req> {
    REQ_ORDER
        .iter()
        .map(|&key| Req { key, status: ReqStatus::Checking, detail: String::new() })
        .collect()
}

/// Requirements needed before recording+transcribing works end to end
/// (Xcrun is optional; Uv only matters until whispermlx exists).
pub fn missing_essential(reqs: &[Req]) -> usize {
    let whisper_ok = reqs
        .iter()
        .any(|r| r.key == ReqKey::Whispermlx && r.status == ReqStatus::Ok);
    reqs.iter()
        .filter(|r| r.status == ReqStatus::Missing)
        .filter(|r| match r.key {
            ReqKey::Xcrun => false,
            ReqKey::Uv => !whisper_ok, // uv is only a means to whispermlx
            _ => true,
        })
        .count()
}

/// A guided install running in the background, output streamed.
pub struct Installer {
    pub target: ReqKey,
    pub lines: Receiver<String>,
    pub done: Receiver<Result<(), String>>,
}

fn brew() -> Result<std::path::PathBuf, String> {
    find_bin("brew").ok_or_else(|| {
        "Homebrew not found — install it from https://brew.sh (or add the tool to your nix config)".into()
    })
}

/// The command sequence that installs a requirement, or an instruction
/// error when it cannot be automated.
fn install_plan(key: ReqKey) -> Result<Vec<Vec<String>>, String> {
    if is_faked(key) && !matches!(key, ReqKey::Mic | ReqKey::HfToken) {
        return Ok(vec![vec!["sleep".into(), "2".into()]]);
    }
    let brew_cmd = |pkg: &str| -> Result<Vec<String>, String> {
        Ok(vec![brew()?.display().to_string(), "install".into(), pkg.into()])
    };
    match key {
        ReqKey::Sox => Ok(vec![brew_cmd("sox")?]),
        ReqKey::Ffmpeg => Ok(vec![brew_cmd("ffmpeg")?]),
        ReqKey::Uv => Ok(vec![brew_cmd("uv")?]),
        ReqKey::Whispermlx => {
            let mut steps = Vec::new();
            let uv = match find_bin("uv") {
                Some(p) => p.display().to_string(),
                None => {
                    steps.push(brew_cmd("uv")?);
                    "uv".into() // on PATH after the brew step
                }
            };
            steps.push(vec![
                uv,
                "tool".into(),
                "install".into(),
                "--python".into(),
                "3.13".into(),
                "--with".into(),
                "numba>=0.61".into(),
                "whispermlx".into(),
            ]);
            Ok(steps)
        }
        ReqKey::Xcrun => Ok(vec![vec!["xcode-select".into(), "--install".into()]]),
        ReqKey::Mic => Err("grant microphone access to your terminal in System Settings → Privacy & Security".into()),
        ReqKey::HfToken => Err("token setup is manual — see the steps shown".into()),
    }
}

pub fn start_install(key: ReqKey) -> Result<Installer, String> {
    let plan = install_plan(key)?;
    let (line_tx, line_rx) = bounded::<String>(512);
    let (done_tx, done_rx) = bounded(1);
    std::thread::spawn(move || {
        for argv in plan {
            let _ = line_tx.try_send(format!("$ {}", argv.join(" ")));
            let mut cmd = Command::new(&argv[0]);
            cmd.args(&argv[1..]).stdin(Stdio::null()).stdout(Stdio::piped()).stderr(Stdio::piped());
            let mut child = match cmd.spawn() {
                Ok(c) => c,
                Err(e) => {
                    let _ = done_tx.send(Err(format!("{}: {e}", argv[0])));
                    return;
                }
            };
            let out = child.stdout.take();
            let err = child.stderr.take();
            let tx1 = line_tx.clone();
            let h1 = std::thread::spawn(move || {
                if let Some(out) = out {
                    for l in BufReader::new(out).lines().map_while(Result::ok) {
                        let _ = tx1.try_send(l);
                    }
                }
            });
            let tx2 = line_tx.clone();
            let h2 = std::thread::spawn(move || {
                if let Some(err) = err {
                    for l in BufReader::new(err).lines().map_while(Result::ok) {
                        let _ = tx2.try_send(l);
                    }
                }
            });
            let status = child.wait();
            let _ = h1.join();
            let _ = h2.join();
            match status {
                Ok(s) if s.success() => {}
                Ok(s) => {
                    let _ = done_tx.send(Err(format!("{} exited: {s}", argv[0])));
                    return;
                }
                Err(e) => {
                    let _ = done_tx.send(Err(e.to_string()));
                    return;
                }
            }
        }
        let _ = done_tx.send(Ok(()));
    });
    Ok(Installer { target: key, lines: line_rx, done: done_rx })
}

/// Manual steps for the gated pyannote token, shown on the wizard.
pub fn hf_token_steps() -> Vec<String> {
    vec![
        "1. accept the model terms at huggingface.co/pyannote/speaker-diarization-community-1".into(),
        "2. create a read token at huggingface.co/settings/tokens".into(),
        "3. in another terminal, run:".into(),
        format!(
            "   mkdir -p ~/.cache/huggingface && printf '%s' 'hf_yourtoken' > {} && chmod 600 {}",
            "~/.cache/huggingface/token", "~/.cache/huggingface/token"
        ),
        "this screen re-checks automatically — the tick goes green when the token lands".into(),
    ]
}

pub fn open_hf_pages() {
    for url in [
        "https://huggingface.co/pyannote/speaker-diarization-community-1",
        "https://huggingface.co/settings/tokens",
    ] {
        let _ = Command::new("open").arg(url).spawn();
    }
}
