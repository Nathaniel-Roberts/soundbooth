use serde::Deserialize;
use std::path::{Path, PathBuf};
use std::time::Duration;

use crate::transcribe::audio_stem;

/// One diarised transcript segment from the whispermlx JSON output.
#[derive(Deserialize, Clone)]
pub struct WSeg {
    #[serde(default)]
    pub start: f64,
    #[serde(default)]
    pub end: f64,
    #[serde(default)]
    pub text: String,
    #[serde(default)]
    pub speaker: String,
}

#[derive(Deserialize)]
struct WDoc {
    #[serde(default)]
    segments: Vec<WSeg>,
}

pub fn load_segments(out_dir: &Path, audio: &Path) -> Vec<WSeg> {
    let path = out_dir.join(format!("{}.json", audio_stem(audio)));
    std::fs::read_to_string(path)
        .ok()
        .and_then(|s| serde_json::from_str::<WDoc>(&s).ok())
        .map(|d| d.segments)
        .unwrap_or_default()
}

/// One diarised speaker, ranked by talk time.
#[derive(Clone)]
pub struct SpkStat {
    pub id: String,
    pub name: String,  // user-assigned; empty until named
    pub quote: String, // longest segment, for identification
    pub dur: f64,
    pub share: f64,
}

pub fn speaker_stats(segs: &[WSeg]) -> Vec<SpkStat> {
    let mut order: Vec<String> = Vec::new();
    let mut stats: Vec<SpkStat> = Vec::new();
    let mut quote_len: std::collections::HashMap<String, f64> = Default::default();
    let mut total = 0.0;
    for s in segs {
        if s.speaker.is_empty() {
            continue;
        }
        if !order.contains(&s.speaker) {
            order.push(s.speaker.clone());
            stats.push(SpkStat { id: s.speaker.clone(), name: String::new(), quote: String::new(), dur: 0.0, share: 0.0 });
        }
        let d = (s.end - s.start).max(0.0);
        total += d;
        let st = stats.iter_mut().find(|x| x.id == s.speaker).unwrap();
        st.dur += d;
        let q = quote_len.entry(s.speaker.clone()).or_insert(0.0);
        if d > *q {
            *q = d;
            st.quote = s.text.trim().to_string();
        }
    }
    stats.sort_by(|a, b| b.dur.partial_cmp(&a.dur).unwrap_or(std::cmp::Ordering::Equal));
    for st in &mut stats {
        st.share = if total > 0.0 { st.dur / total } else { 0.0 };
    }
    stats
}

/// Assigned name, else "Speaker N" in talk-time order.
pub fn display_name(stats: &[SpkStat], id: &str) -> String {
    for (i, s) in stats.iter().enumerate() {
        if s.id == id {
            return if s.name.is_empty() { format!("Speaker {}", i + 1) } else { s.name.clone() };
        }
    }
    id.to_string()
}

pub fn fmt_clock(seconds: f64) -> String {
    let s = seconds.max(0.0) as u64;
    if s >= 3600 {
        format!("{}:{:02}:{:02}", s / 3600, (s % 3600) / 60, s % 60)
    } else {
        format!("{:02}:{:02}", s / 60, s % 60)
    }
}

/// Render the diarised segments as readable markdown with speaker names,
/// merged turns, and markers woven in. Returns the written path.
pub fn write_named_transcript(
    out_dir: &Path,
    audio: &Path,
    segs: &[WSeg],
    stats: &[SpkStat],
    markers: &[Duration],
) -> Result<PathBuf, String> {
    let stem = audio_stem(audio);
    let path = out_dir.join(format!("{stem}-transcript.md"));
    let mut b = String::new();
    b.push_str(&format!("# Transcript — {stem}\n\n"));
    b.push_str(&format!("Source: {}\n\nSpeakers: ", audio.file_name().map(|n| n.to_string_lossy().into_owned()).unwrap_or_default()));
    for (i, s) in stats.iter().enumerate() {
        if i > 0 {
            b.push_str(", ");
        }
        b.push_str(&format!("{} ({}, {:.0}%)", display_name(stats, &s.id), fmt_clock(s.dur), s.share * 100.0));
    }
    b.push_str("\n\n---\n\n");

    let mut next_marker = 0usize;
    let write_markers_up_to = |b: &mut String, t: f64, next: &mut usize| {
        while *next < markers.len() && markers[*next].as_secs_f64() <= t {
            b.push_str(&format!("**— marker {} at {} —**\n\n", *next + 1, fmt_clock(markers[*next].as_secs_f64())));
            *next += 1;
        }
    };

    let mut cur_speaker = String::new();
    let mut cur_start = 0.0;
    let mut cur_text = String::new();
    let flush = |b: &mut String, sp: &str, start: f64, text: &mut String| {
        if !text.trim().is_empty() {
            b.push_str(&format!("{} [{}]: {}\n\n", display_name(stats, sp), fmt_clock(start), text.trim()));
        }
        text.clear();
    };
    for s in segs {
        write_markers_up_to(&mut b, s.start, &mut next_marker);
        if s.speaker != cur_speaker {
            flush(&mut b, &cur_speaker, cur_start, &mut cur_text);
            cur_speaker = s.speaker.clone();
            cur_start = s.start;
        }
        cur_text.push(' ');
        cur_text.push_str(s.text.trim());
    }
    flush(&mut b, &cur_speaker, cur_start, &mut cur_text);
    write_markers_up_to(&mut b, f64::MAX, &mut next_marker);

    std::fs::write(&path, b).map_err(|e| e.to_string())?;
    Ok(path)
}

/// Record marker timestamps next to the audio; returns the path if any.
pub fn write_markers_file(audio: &Path, markers: &[Duration]) -> Option<PathBuf> {
    if markers.is_empty() {
        return None;
    }
    let path = audio.with_extension("").with_file_name(format!("{}-markers.txt", audio_stem(audio)));
    let mut b = String::new();
    for (i, m) in markers.iter().enumerate() {
        b.push_str(&format!("marker {}  {}\n", i + 1, fmt_clock(m.as_secs_f64())));
    }
    std::fs::write(&path, b).ok()?;
    Some(path)
}

/// Parse a "-markers.txt" back into durations.
pub fn load_markers_file(audio: &Path) -> Vec<Duration> {
    let path = audio.with_extension("").with_file_name(format!("{}-markers.txt", audio_stem(audio)));
    let Ok(data) = std::fs::read_to_string(path) else {
        return Vec::new();
    };
    let mut out = Vec::new();
    for line in data.lines() {
        let Some(last) = line.split_whitespace().last() else { continue };
        let mut secs = 0u64;
        let mut ok = true;
        for part in last.split(':') {
            match part.parse::<u64>() {
                Ok(v) => secs = secs * 60 + v,
                Err(_) => {
                    ok = false;
                    break;
                }
            }
        }
        if ok && last.contains(':') {
            out.push(Duration::from_secs(secs));
        }
    }
    out
}
