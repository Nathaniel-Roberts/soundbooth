use crossbeam_channel::{bounded, Receiver};
use std::io::{BufRead, BufReader};
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};

use crate::config::hf_token_path;
use crate::state::find_bin;

pub const STAGE_NAMES: [&str; 5] = ["load model", "voice activity", "transcribe", "align", "diarise"];

pub fn audio_stem(audio: &Path) -> String {
    audio.file_stem().map(|s| s.to_string_lossy().into_owned()).unwrap_or_default()
}

pub fn tx_dir_for(audio: &Path) -> PathBuf {
    audio.with_extension("")
}

/// Streams whispermlx output while it runs.
pub struct Transcriber {
    pub lines: Receiver<String>,
    pub done: Receiver<Result<(), String>>,
    pub out_dir: PathBuf,
}

pub fn start_transcribe(
    file: &Path,
    model: &str,
    language: &str,
    speakers: u32,
) -> Result<Transcriber, String> {
    let wmlx = find_bin("whispermlx").ok_or(
        "whispermlx not found — install with: uv tool install --python 3.13 --with 'numba>=0.61' whispermlx",
    )?;
    let token = std::fs::read_to_string(hf_token_path())
        .map_err(|_| format!("no Hugging Face token at {} (needed for pyannote diarisation)", hf_token_path().display()))?;
    let out_dir = tx_dir_for(file);
    std::fs::create_dir_all(&out_dir).map_err(|e| e.to_string())?;

    let mut cmd = Command::new(wmlx);
    cmd.arg(file)
        .args(["--model", model, "--diarize"])
        .args(["--hf_token", token.trim()])
        .args(["--output_format", "all"])
        .arg("--output_dir")
        .arg(&out_dir)
        // torchcodec in that env can't load against torch 2.8; pyannote
        // falls back to its own loader. Silence the warning wall.
        .env("PYTHONWARNINGS", "ignore")
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .stdin(Stdio::null());
    if !language.is_empty() && language != "auto" {
        cmd.args(["--language", language]);
    }
    if speakers > 0 {
        let s = speakers.to_string();
        cmd.args(["--min_speakers", &s, "--max_speakers", &s]);
    }
    let mut child = cmd.spawn().map_err(|e| e.to_string())?;
    let stdout = child.stdout.take().ok_or("stdout missing")?;
    let stderr = child.stderr.take().ok_or("stderr missing")?;

    let (line_tx, line_rx) = bounded::<String>(512);
    let (done_tx, done_rx) = bounded(1);

    // interleave both pipes; split on \r as well as \n so tqdm progress
    // percentages stream live
    let tx2 = line_tx.clone();
    std::thread::spawn(move || read_cr_lines(stdout, line_tx));
    let err_handle = std::thread::spawn(move || read_cr_lines(stderr, tx2));

    std::thread::spawn(move || {
        let status = child.wait();
        let _ = err_handle.join();
        let res = match status {
            Ok(s) if s.success() => Ok(()),
            Ok(s) => Err(format!("whispermlx exited: {s}")),
            Err(e) => Err(e.to_string()),
        };
        let _ = done_tx.send(res);
    });

    Ok(Transcriber { lines: line_rx, done: done_rx, out_dir })
}

fn read_cr_lines(r: impl std::io::Read, tx: crossbeam_channel::Sender<String>) {
    let mut reader = BufReader::new(r);
    let mut buf = Vec::new();
    loop {
        buf.clear();
        // read until \n, then split any embedded \r segments
        match reader.read_until(b'\n', &mut buf) {
            Ok(0) | Err(_) => return,
            Ok(_) => {
                for part in buf.split(|&b| b == b'\r' || b == b'\n') {
                    let s = String::from_utf8_lossy(part).trim().to_string();
                    if !s.is_empty() && tx.try_send(s).is_err() {
                        // drop on backpressure; the raw log is best-effort
                    }
                }
            }
        }
    }
}

/// Which pipeline stage a whispermlx output line indicates, if any.
pub fn stage_of(line: &str) -> Option<usize> {
    if line.contains("Performing diarization") || line.contains("Loading diarization model") {
        Some(4)
    } else if line.contains("Performing alignment") {
        Some(3)
    } else if line.contains("Performing transcription") || line.contains("Transcribing:") {
        Some(2)
    } else if line.contains("voice activity detection") {
        Some(1)
    } else if line.contains("Loading MLX Whisper model") {
        Some(0)
    } else {
        None
    }
}

/// Percentage from a tqdm "Transcribing:  42%|..." line.
pub fn pct_of(line: &str) -> Option<f64> {
    let idx = line.find("Transcribing:")?;
    let rest = &line[idx + "Transcribing:".len()..];
    let pct_pos = rest.find('%')?;
    rest[..pct_pos].trim().parse::<f64>().ok().map(|p| p / 100.0)
}

/// Text from a live "[2.17 --> 3.50] words" segment line.
pub fn seg_text_of(line: &str) -> Option<&str> {
    let line = line.trim();
    if !line.starts_with('[') {
        return None;
    }
    let close = line.find(']')?;
    let inner = &line[1..close];
    if !inner.contains("-->") {
        return None;
    }
    let text = line[close + 1..].trim();
    if text.is_empty() {
        None
    } else {
        Some(text)
    }
}
