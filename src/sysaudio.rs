use crossbeam_channel::{bounded, Receiver};
use std::io::{BufRead, BufReader, Read, Write};
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::time::Duration;

use crate::audio::{lvl, MeterTick, CLIP_THRESHOLD, TICK_HZ};
use crate::state::{find_bin, state_dir};

/// ScreenCaptureKit system-audio tap (see helpers/systap.swift): captures
/// all app audio with only the Screen Recording permission, streams raw
/// f32le interleaved stereo 48 kHz on stdout.
const SYSTAP_SOURCE: &str = include_str!("../helpers/systap.swift");
const SYS_RATE: u32 = 48000;

fn source_hash() -> u64 {
    // FNV-1a: stable across runs (std's hasher is seeded per-process)
    let mut h: u64 = 0xcbf29ce484222325;
    for b in SYSTAP_SOURCE.bytes() {
        h ^= b as u64;
        h = h.wrapping_mul(0x100000001b3);
    }
    h
}

/// Compile the helper on first use; cache by source hash.
pub fn systap_binary() -> Result<PathBuf, String> {
    let bin_dir = state_dir().join("bin");
    let bin = bin_dir.join(format!("systap-{:012x}", source_hash() & 0xffff_ffff_ffff));
    if bin.exists() {
        return Ok(bin);
    }
    if find_bin("xcrun").is_none() {
        return Err("system audio needs the Xcode command line tools (xcode-select --install)".into());
    }
    std::fs::create_dir_all(&bin_dir).map_err(|e| e.to_string())?;
    let src = bin_dir.join("systap.swift");
    std::fs::write(&src, SYSTAP_SOURCE).map_err(|e| e.to_string())?;
    let out = Command::new("xcrun")
        .args(["swiftc", "-O", "-o"])
        .arg(&bin)
        .arg(&src)
        .output()
        .map_err(|e| e.to_string())?;
    if !out.status.success() {
        return Err(format!(
            "compiling system audio helper: {}",
            String::from_utf8_lossy(&out.stderr).lines().next().unwrap_or("")
        ));
    }
    Ok(bin)
}

/// Records system audio to a mono FLAC and meters it for the UI.
pub struct SysCapture {
    pub ticks: Receiver<MeterTick>,
    pub ready: Receiver<Result<(), String>>,
    pub file: PathBuf,
    tap: Child,
    enc_done: Receiver<Result<(), String>>,
}

pub fn start_sys_capture(file: PathBuf) -> Result<SysCapture, String> {
    let bin = systap_binary()?;
    let sox = find_bin("sox").ok_or("sox not found")?;

    let mut tap = Command::new(&bin)
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .map_err(|e| format!("starting system tap: {e}"))?;
    let tap_out = tap.stdout.take().ok_or("tap stdout missing")?;
    let tap_err = tap.stderr.take().ok_or("tap stderr missing")?;

    // stereo f32 in -> mono mixdown FLAC out
    let mut enc = Command::new(&sox)
        .args(["-q", "-t", "raw", "-e", "floating-point", "-b", "32", "-c", "2"])
        .args(["-r", &SYS_RATE.to_string(), "-"])
        .arg(&file)
        .args(["remix", "1,2"])
        .stdin(Stdio::piped())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .map_err(|e| format!("starting system encoder: {e}"))?;
    let mut enc_in = enc.stdin.take().ok_or("encoder stdin missing")?;

    let (tick_tx, tick_rx) = bounded::<MeterTick>(256);
    let (ready_tx, ready_rx) = bounded::<Result<(), String>>(2);
    let (done_tx, done_rx) = bounded::<Result<(), String>>(1);

    // readiness / TCC watcher on the tap's stderr
    std::thread::spawn(move || {
        let reader = BufReader::new(tap_err);
        for line in reader.lines().map_while(Result::ok) {
            if line.contains("SB_READY") {
                let _ = ready_tx.try_send(Ok(()));
            } else if line.contains("SB_ERR") {
                let msg = line.trim_start_matches("SB_ERR:").trim().to_string();
                let _ = ready_tx.try_send(Err(msg));
                return;
            }
        }
    });

    // PCM pump: tee to the encoder, fold into 25 ms meter ticks
    std::thread::spawn(move || {
        let frames_per_tick = (SYS_RATE / TICK_HZ) as usize;
        let mut reader = tap_out;
        let mut buf = [0u8; 32768];
        let mut carry: Vec<u8> = Vec::new();
        let (mut frames, mut sum2, mut peak, mut clip) = (0usize, 0f64, 0f64, false);
        loop {
            let n = match reader.read(&mut buf) {
                Ok(0) | Err(_) => break,
                Ok(n) => n,
            };
            if enc_in.write_all(&buf[..n]).is_err() {
                break;
            }
            carry.extend_from_slice(&buf[..n]);
            let usable = carry.len() / 8 * 8; // one stereo f32 frame = 8 bytes
            for i in (0..usable).step_by(8) {
                let l = f32::from_le_bytes([carry[i], carry[i + 1], carry[i + 2], carry[i + 3]]) as f64;
                let r = f32::from_le_bytes([carry[i + 4], carry[i + 5], carry[i + 6], carry[i + 7]]) as f64;
                let v = (l + r) / 2.0;
                let av = v.abs();
                if av > peak { peak = av }
                if av >= CLIP_THRESHOLD { clip = true }
                sum2 += v * v;
                frames += 1;
                if frames >= frames_per_tick {
                    let tick = MeterTick {
                        rms: lvl((sum2 / frames as f64).sqrt()),
                        peak: lvl(peak),
                        rms_r: lvl((sum2 / frames as f64).sqrt()),
                        peak_r: lvl(peak),
                        db: if peak > 0.0 { 20.0 * peak.log10() } else { -99.0 },
                        clip,
                    };
                    let _ = tick_tx.try_send(tick);
                    frames = 0; sum2 = 0.0; peak = 0.0; clip = false;
                }
            }
            carry.drain(..usable);
        }
        drop(enc_in);
        let res = match enc.wait() {
            Ok(s) if s.success() => Ok(()),
            Ok(s) => Err(format!("system encoder exited: {s}")),
            Err(e) => Err(e.to_string()),
        };
        let _ = done_tx.send(res);
    });

    Ok(SysCapture { ticks: tick_rx, ready: ready_rx, file, tap, enc_done: done_rx })
}

impl SysCapture {
    /// Finalise the system track.
    pub fn stop(mut self) {
        unsafe { libc::kill(self.tap.id() as i32, libc::SIGINT) };
        let deadline = std::time::Instant::now() + Duration::from_secs(3);
        loop {
            match self.tap.try_wait() {
                Ok(Some(_)) => break,
                _ if std::time::Instant::now() > deadline => {
                    let _ = self.tap.kill();
                    let _ = self.tap.wait();
                    break;
                }
                _ => std::thread::sleep(Duration::from_millis(30)),
            }
        }
        let _ = self.enc_done.recv_timeout(Duration::from_secs(3));
    }
}

/// Combine a mono mic track (left) and mono system track (right) into one
/// stereo file.
pub fn merge_mic_system(mic: &Path, sys: &Path, out: &Path) -> Result<(), String> {
    let sox = find_bin("sox").ok_or("sox not found")?;
    let o = Command::new(sox)
        .arg("-M")
        .arg(mic)
        .arg(sys)
        .arg(out)
        .output()
        .map_err(|e| e.to_string())?;
    if !o.status.success() {
        return Err(format!(
            "merging tracks: {}",
            String::from_utf8_lossy(&o.stderr).lines().next().unwrap_or("")
        ));
    }
    Ok(())
}
