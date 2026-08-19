use std::path::{Path, PathBuf};
use std::time::Duration;

use crate::audio::{lvl, Capture, CaptureCfg};
use crate::captions::write_wav;
use crate::player::decode_wave;
use crate::search::{hit_context, load_library, search_transcripts};
use crate::speakers::{load_markers_file, write_markers_file};
use crate::state::{concat_flac, find_bin, is_own_recording, sweep_retention};
use crate::sysaudio::{merge_mic_system, start_sys_capture, systap_binary};
use crate::waveform::{downsample, wrap_text, WaveCol};

fn tmp_dir(tag: &str) -> PathBuf {
    let d = std::env::temp_dir().join(format!("sb-test-{tag}-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&d);
    std::fs::create_dir_all(&d).unwrap();
    d
}

fn hw() -> bool {
    std::env::var("SOUNDBOOTH_HW_TEST").as_deref() == Ok("1")
}

fn set_mtime_days_ago(p: &Path, days: u64) {
    use std::ffi::CString;
    let secs = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_secs()
        - days * 24 * 3600;
    let tv = libc::timeval { tv_sec: secs as i64, tv_usec: 0 };
    let times = [tv, tv];
    let c = CString::new(p.to_str().unwrap()).unwrap();
    unsafe { libc::utimes(c.as_ptr(), times.as_ptr()) };
}

#[test]
fn lvl_mapping() {
    assert_eq!(lvl(0.0), 0.0);
    assert_eq!(lvl(1.0), 1.0);
    assert!(lvl(0.00316) < 0.01); // -50 dBFS floor
}

#[test]
fn own_recording_pattern() {
    assert!(is_own_recording("meeting-20260817-120415.flac"));
    assert!(is_own_recording("a-b c-20260817-120415.flac"));
    assert!(!is_own_recording("holiday-song.flac"));
    assert!(!is_own_recording("meeting-2026087-120415.flac"));
    assert!(!is_own_recording("meeting-20260817-120415.wav"));
}

#[test]
fn retention_sweep() {
    let dir = tmp_dir("retention");
    let old = dir.join("meeting-20250101-090000.flac");
    let fresh = dir.join("meeting-20991231-090000.flac");
    let foreign = dir.join("holiday-song.flac");
    let tx = dir.join("meeting-20250101-090000");
    for f in [&old, &fresh, &foreign] {
        std::fs::write(f, "x").unwrap();
    }
    std::fs::create_dir_all(&tx).unwrap();
    set_mtime_days_ago(&old, 40);
    set_mtime_days_ago(&foreign, 40);

    assert_eq!(sweep_retention(dir.to_str().unwrap(), 30), 1);
    assert!(!old.exists(), "old recording should be deleted");
    assert!(fresh.exists(), "fresh recording should survive");
    assert!(foreign.exists(), "non-soundbooth flac must never be touched");
    assert!(tx.exists(), "transcript dir must be kept");
    assert_eq!(sweep_retention(dir.to_str().unwrap(), 0), 0);
}

#[test]
fn markers_roundtrip() {
    let dir = tmp_dir("markers");
    let audio = dir.join("meet-20260817-000000.flac");
    let markers = [Duration::from_secs(65), Duration::from_secs(3725)];
    let path = write_markers_file(&audio, &markers).expect("markers written");
    assert!(path.exists());
    let back = load_markers_file(&audio);
    assert_eq!(back, markers.to_vec());
}

#[test]
fn transcript_search() {
    let dir = tmp_dir("search");
    let audio = dir.join("standup-20260817-090000.flac");
    let tx = dir.join("standup-20260817-090000");
    std::fs::write(&audio, "x").unwrap();
    std::fs::create_dir_all(&tx).unwrap();
    std::fs::write(
        tx.join("standup-20260817-090000.txt"),
        "Scott talked about the kiosk rollout.\nBen covered the SCEP profiles.\nNathaniel raised the Palo rulebase.\n",
    )
    .unwrap();

    let lib = load_library(dir.to_str().unwrap());
    assert_eq!(lib.len(), 1);
    assert!(lib[0].has_tx);
    let hits = search_transcripts(&lib, "scep");
    assert_eq!(hits.len(), 1);
    assert_eq!(hits[0].line_no, 2);
    assert_eq!(hit_context(&hits[0], 1).len(), 3);
    assert!(search_transcripts(&lib, "zzz-nope").is_empty());
}

#[test]
fn wrap_and_downsample() {
    let lines = wrap_text("one two three four five six", 9);
    assert!(lines.iter().all(|l| l.chars().count() <= 9));
    assert_eq!(lines.join(" "), "one two three four five six");

    let mut cols = vec![WaveCol::default(); 10];
    for (i, c) in cols.iter_mut().enumerate() {
        c.peak = i as f64 / 10.0;
    }
    cols[9].clip = true;
    let out = downsample(&cols, 5);
    assert_eq!(out.len(), 2);
    assert!((out[1].peak - 0.9).abs() < 1e-9, "max-pool lost the peak");
    assert!(out[1].clip, "max-pool lost the clip flag");
    assert_eq!(downsample(&cols, 1).len(), 10);
}

#[test]
fn wav_writer() {
    let dir = tmp_dir("wav");
    let path = dir.join("t.wav");
    let pcm = vec![0i16; 1600];
    write_wav(&path, &pcm, 16000).unwrap();
    let Some(soxi) = find_bin("soxi") else { return };
    let out = std::process::Command::new(soxi).arg("-r").arg(&path).output().unwrap();
    assert_eq!(String::from_utf8_lossy(&out.stdout).trim(), "16000");
}

fn synth(path: &Path, secs: u32, freq: u32) {
    let sox = find_bin("sox").expect("sox required");
    let st = std::process::Command::new(sox)
        .args(["-n", "-r", "48000", "-c", "1"])
        .arg(path)
        .args(["synth", &secs.to_string(), "sine", &freq.to_string(), "vol", "0.5"])
        .status()
        .unwrap();
    assert!(st.success());
}

#[test]
fn decode_synth_wave() {
    if find_bin("sox").is_none() {
        return;
    }
    let dir = tmp_dir("decode");
    let f = dir.join("tone.flac");
    synth(&f, 3, 440);
    let rx = decode_wave(f, 120);
    let dw = rx.recv_timeout(Duration::from_secs(20)).unwrap().unwrap();
    assert!((dw.dur - 3.0).abs() < 0.2, "duration {}", dw.dur);
    assert!(dw.cols.len() >= 100 && dw.cols.len() <= 120, "cols {}", dw.cols.len());
    let loud = dw.cols.iter().filter(|c| c.peak > 0.5).count();
    assert!(loud > dw.cols.len() / 2, "sine should be loud: {loud}/{}", dw.cols.len());
}

#[test]
fn concat_and_merge() {
    if find_bin("sox").is_none() {
        return;
    }
    let dir = tmp_dir("concat");
    let a = dir.join("seg-000.flac");
    let b = dir.join("seg-001.flac");
    synth(&a, 1, 440);
    synth(&b, 1, 880);
    let out = dir.join("joined.flac");
    concat_flac(&[a.clone(), b.clone()], &out).unwrap();
    assert!(out.exists());

    let merged = dir.join("merged.flac");
    merge_mic_system(&a, &b, &merged).unwrap();
    let soxi = find_bin("soxi").unwrap();
    let ch = std::process::Command::new(soxi).arg("-c").arg(&merged).output().unwrap();
    assert_eq!(String::from_utf8_lossy(&ch.stdout).trim(), "2");
}

// ---------------------------------------------------------------------
// hardware tests: SOUNDBOOTH_HW_TEST=1 cargo test -- --test-threads=1

#[test]
fn hw_capture_record() {
    if !hw() {
        return;
    }
    let dir = tmp_dir("hwcap");
    let file = dir.join("cap.flac");
    let cap = Capture::start(
        crate::config::DEFAULT_DEVICE,
        CaptureCfg { encode_to: Some(file.clone()), target_channels: 1, captions: true },
    )
    .expect("capture start");
    std::thread::sleep(Duration::from_secs(3));
    let ticks = cap.ticks.try_iter().count();
    let cap_pcm: usize = cap.caption_pcm.try_iter().map(|v| v.len()).sum();
    cap.stop().expect("capture stop");
    assert!(ticks > 80, "expected ~120 ticks in 3s, got {ticks}");
    // ~3 s of 16 kHz caption PCM
    assert!(cap_pcm > 30000, "caption pcm too small: {cap_pcm}");
    let md = std::fs::metadata(&file).expect("recording exists");
    assert!(md.len() > 0);
}

#[test]
fn hw_capture_pause_resume() {
    if !hw() {
        return;
    }
    let dir = tmp_dir("hwpause");
    let file = dir.join("p.flac");
    let cap = Capture::start(
        crate::config::DEFAULT_DEVICE,
        CaptureCfg { encode_to: Some(file.clone()), target_channels: 1, captions: false },
    )
    .unwrap();
    std::thread::sleep(Duration::from_millis(1500));
    cap.pause();
    assert!(cap.paused());
    let at_pause = cap.elapsed();
    std::thread::sleep(Duration::from_secs(1));
    assert!(cap.elapsed() - at_pause < Duration::from_millis(100), "elapsed advanced while paused");
    cap.resume();
    std::thread::sleep(Duration::from_millis(1500));
    let total = cap.elapsed();
    cap.stop().unwrap();
    assert!(total > Duration::from_millis(2500) && total < Duration::from_millis(3700), "elapsed {total:?}");
    assert!(std::fs::metadata(&file).unwrap().len() > 0);
}

#[test]
fn hw_systap() {
    if !hw() || find_bin("xcrun").is_none() {
        return;
    }
    systap_binary().expect("helper compiles");
    let dir = tmp_dir("hwsys");
    let sys = start_sys_capture(dir.join("sys.flac")).expect("tap start");
    match sys.ready.recv_timeout(Duration::from_secs(6)) {
        Ok(Ok(())) => {
            std::thread::sleep(Duration::from_secs(2));
            let file = sys.file.clone();
            sys.stop();
            assert!(std::fs::metadata(&file).unwrap().len() > 0);
        }
        _ => {
            // no Screen Recording permission here: the fallback path
            sys.stop();
        }
    }
}

#[test]
fn hw_spooler() {
    if !hw() || find_bin("ffmpeg").is_none() {
        return;
    }
    let sp = crate::spool::start_spooler(None, 1, Duration::from_secs(60)).expect("spooler");
    std::thread::sleep(Duration::from_secs(3));
    sp.trigger();
    std::thread::sleep(Duration::from_secs(2));
    let dir = sp.dir.clone();
    let segs = sp.stop();
    assert!(!segs.is_empty(), "no segments after trigger+stop");
    let out = tmp_dir("hwspool").join("spooled.flac");
    concat_flac(&segs, &out).unwrap();
    assert!(std::fs::metadata(&out).unwrap().len() > 0);
    crate::spool::Spooler::cleanup_dir(&dir);
}

#[test]
fn json_nan_and_word_speakers() {
    let dir = tmp_dir("nanjson");
    let audio = dir.join("meet-20260817-000000.flac");
    let tx = dir.join("meet-20260817-000000");
    std::fs::create_dir_all(&tx).unwrap();
    // real-world whispermlx quirks: bare NaN scores (invalid JSON), one
    // segment with no segment-level speaker (words only), and "NaN"
    // appearing inside transcript text
    let json = r#"{"segments": [
      {"start": 0.0, "end": 1.0, "text": "My name is Scott.", "avg_logprob": NaN,
       "speaker": "SPEAKER_01", "words": [{"word": "My", "score": NaN, "speaker": "SPEAKER_01"}]},
      {"start": 1.0, "end": 2.5, "text": "the value was NaN today",
       "words": [{"word": "the", "speaker": "SPEAKER_00"}, {"word": "value", "speaker": "SPEAKER_00"}, {"word": "was", "speaker": "SPEAKER_01"}]}
    ], "language": "en"}"#;
    std::fs::write(tx.join("meet-20260817-000000.json"), json).unwrap();
    let segs = crate::speakers::load_segments(&tx, &audio);
    assert_eq!(segs.len(), 2, "NaN scores must not sink the parse");
    assert_eq!(segs[0].speaker, "SPEAKER_01");
    assert_eq!(segs[1].speaker, "SPEAKER_00", "word-majority speaker fallback");
    assert_eq!(segs[1].text, "the value was NaN today", "text must be untouched");
    let stats = crate::speakers::speaker_stats(&segs);
    assert_eq!(stats.len(), 2);
}

#[test]
fn doctor_checks_and_simulation() {
    use crate::doctor::*;
    // this machine has everything: essentials should be zero
    let mut reqs = all_reqs();
    for r in &mut reqs {
        let (st, d) = check(r.key);
        r.status = st;
        r.detail = d;
    }
    assert_eq!(missing_essential(&reqs), 0, "dev machine should be fully set up");
    // uv only counts while whispermlx is absent
    for r in &mut reqs {
        if r.key == ReqKey::Uv {
            r.status = ReqStatus::Missing;
        }
    }
    assert_eq!(missing_essential(&reqs), 0, "uv missing is fine once whispermlx exists");
    for r in &mut reqs {
        if r.key == ReqKey::Whispermlx {
            r.status = ReqStatus::Missing;
        }
    }
    assert_eq!(missing_essential(&reqs), 2, "whispermlx missing drags uv back in");
    // xcrun is optional
    for r in &mut reqs {
        if r.key == ReqKey::Xcrun {
            r.status = ReqStatus::Missing;
        }
    }
    assert_eq!(missing_essential(&reqs), 2);
}
