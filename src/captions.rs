use crossbeam_channel::{bounded, Receiver, Sender};
use std::io::{BufRead, BufReader, Write};
use std::path::PathBuf;
use std::process::{Child, Command, Stdio};

/// Live captions: a persistent python daemon keeps a small whisper model
/// warm on the GPU; the Capture engine hands us 16 kHz mono PCM which we
/// batch into 5-second silence-gated WAV chunks.
const CAPTION_REPO: &str = "mlx-community/whisper-base-mlx";
const CAPTION_RATE: usize = 16000;
const CAPTION_SECONDS: usize = 5;
/// chunks quieter than this peak are skipped: whisper hallucinates
/// plausible text on silence (~ -42 dBFS)
const CAPTION_MIN_PEAK: i16 = 260;

fn caption_script() -> String {
    format!(
        r#"
import json, sys
import mlx_whisper
sys.stderr.write("SB_CAPTIONS_READY\n"); sys.stderr.flush()
for line in sys.stdin:
    path = line.strip()
    if not path:
        continue
    try:
        r = mlx_whisper.transcribe(path, path_or_hf_repo="{CAPTION_REPO}", language="en")
        print(json.dumps({{"text": r.get("text", "").strip()}}), flush=True)
    except Exception as e:
        print(json.dumps({{"err": str(e)}}), flush=True)
"#
    )
}

pub fn caption_python() -> Result<PathBuf, String> {
    let p = dirs::home_dir()
        .unwrap_or_default()
        .join(".local/share/uv/tools/whispermlx/bin/python");
    if p.exists() {
        Ok(p)
    } else {
        Err("whispermlx python env not found (install whispermlx first)".into())
    }
}

pub struct Captioner {
    pub lines: Receiver<String>,
    pcm_tx: Sender<Vec<i16>>,
    daemon: Child,
    tmp_dir: PathBuf,
}

pub fn start_captioner() -> Result<Captioner, String> {
    let python = caption_python()?;
    let tmp_dir = std::env::temp_dir().join(format!("soundbooth-cap-{}", std::process::id()));
    std::fs::create_dir_all(&tmp_dir).map_err(|e| e.to_string())?;

    let mut daemon = Command::new(python)
        .arg("-c")
        .arg(caption_script())
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .spawn()
        .map_err(|e| format!("starting caption model: {e}"))?;
    let mut daemon_in = daemon.stdin.take().ok_or("daemon stdin missing")?;
    let daemon_out = daemon.stdout.take().ok_or("daemon stdout missing")?;

    let (pcm_tx, pcm_rx) = bounded::<Vec<i16>>(256);
    let (line_tx, line_rx) = bounded::<String>(32);

    // batcher: accumulate 5 s, gate on silence, write WAV, notify daemon
    let dir = tmp_dir.clone();
    std::thread::spawn(move || {
        let chunk_samples = CAPTION_RATE * CAPTION_SECONDS;
        let mut acc: Vec<i16> = Vec::with_capacity(chunk_samples);
        let mut n = 0usize;
        while let Ok(pcm) = pcm_rx.recv() {
            acc.extend_from_slice(&pcm);
            while acc.len() >= chunk_samples {
                let chunk: Vec<i16> = acc.drain(..chunk_samples).collect();
                let peak = chunk.iter().map(|s| s.unsigned_abs()).max().unwrap_or(0);
                if peak >= CAPTION_MIN_PEAK as u16 {
                    let path = dir.join(format!("chunk-{n}.wav"));
                    n += 1;
                    if write_wav(&path, &chunk, CAPTION_RATE as u32).is_ok()
                        && writeln!(daemon_in, "{}", path.display()).is_err()
                    {
                        return;
                    }
                }
            }
        }
        // channel closed: end the daemon loop
        drop(daemon_in);
    });

    // results
    let dir2 = tmp_dir.clone();
    std::thread::spawn(move || {
        let reader = BufReader::new(daemon_out);
        let mut served = 0usize;
        for line in reader.lines().map_while(Result::ok) {
            let _ = std::fs::remove_file(dir2.join(format!("chunk-{served}.wav")));
            served += 1;
            if let Ok(v) = serde_json::from_str::<serde_json::Value>(&line) {
                if let Some(text) = v.get("text").and_then(|t| t.as_str()) {
                    if !text.is_empty() {
                        let _ = line_tx.try_send(text.to_string());
                    }
                }
            }
        }
    });

    Ok(Captioner { lines: line_rx, pcm_tx, daemon, tmp_dir })
}

impl Captioner {
    /// Feed 16 kHz mono PCM from the capture engine.
    pub fn push_pcm(&self, pcm: Vec<i16>) {
        let _ = self.pcm_tx.try_send(pcm);
    }

    pub fn stop(mut self) {
        drop(self.pcm_tx); // batcher exits, closing the daemon's stdin
        let daemon_id = self.daemon.id();
        let dir = self.tmp_dir.clone();
        std::thread::spawn(move || {
            std::thread::sleep(std::time::Duration::from_secs(3));
            unsafe { libc::kill(daemon_id as i32, libc::SIGKILL) };
            let _ = std::fs::remove_dir_all(dir);
        });
        std::thread::spawn(move || {
            let _ = self.daemon.wait();
        });
    }
}

/// Minimal mono 16-bit WAV writer.
pub fn write_wav(path: &std::path::Path, pcm: &[i16], rate: u32) -> std::io::Result<()> {
    let data_len = (pcm.len() * 2) as u32;
    let mut h = Vec::with_capacity(44 + pcm.len() * 2);
    h.extend_from_slice(b"RIFF");
    h.extend_from_slice(&(36 + data_len).to_le_bytes());
    h.extend_from_slice(b"WAVEfmt ");
    h.extend_from_slice(&16u32.to_le_bytes());
    h.extend_from_slice(&1u16.to_le_bytes()); // PCM
    h.extend_from_slice(&1u16.to_le_bytes()); // mono
    h.extend_from_slice(&rate.to_le_bytes());
    h.extend_from_slice(&(rate * 2).to_le_bytes());
    h.extend_from_slice(&2u16.to_le_bytes());
    h.extend_from_slice(&16u16.to_le_bytes());
    h.extend_from_slice(b"data");
    h.extend_from_slice(&data_len.to_le_bytes());
    for s in pcm {
        h.extend_from_slice(&s.to_le_bytes());
    }
    std::fs::write(path, h)
}
