use crossbeam_channel::Receiver;
use crossterm::event::{KeyCode, KeyEvent, KeyModifiers};
use std::path::PathBuf;
use std::time::{Duration, Instant};

use crate::audio::{Capture, CaptureCfg, MeterTick, TICK_HZ};
use crate::captions::{start_captioner, Captioner};
use crate::config::{self, Config, DEFAULT_DEVICE};
use crate::control;
use crate::player::{decode_wave, start_playback, DecodedWave, Playback};
use crate::search::{delete_recording, hit_context, load_library, search_transcripts, LibEntry, SearchHit};
use crate::speakers::{load_markers_file, load_segments, speaker_stats, write_markers_file, write_named_transcript, SpkStat, WSeg};
use crate::spool::{av_index_for, start_spooler, Spooler};
use crate::state::{find_orphans, free_bytes, sessions_dir, sweep_retention, sweep_stale, Orphan};
use crate::sysaudio::{merge_mic_system, start_sys_capture, SysCapture};
use crate::theme::{apply_overrides, by_name, theme_names, Theme};
use crate::transcribe::{pct_of, seg_text_of, stage_of, start_transcribe, tx_dir_for, Transcriber};
use crate::waveform::WaveCol;

#[derive(PartialEq, Clone, Copy)]
pub enum Screen {
    Setup,
    Armed,
    Recording,
    Transcribing,
    Speakers,
    Library,
    Done,
}

// setup form fields, in display order
pub const F_DEVICE: usize = 0;
pub const F_OUT_DIR: usize = 1;
pub const F_NAME: usize = 2;
pub const F_CHANNELS: usize = 3;
pub const F_SYS_AUDIO: usize = 4;
pub const F_MODE: usize = 5;
pub const F_BUFFER: usize = 6;
pub const F_TRANSCRIBE: usize = 7;
pub const F_CAPTIONS: usize = 8;
pub const F_MODEL: usize = 9;
pub const F_SPEAKERS: usize = 10;
pub const F_LANGUAGE: usize = 11;
pub const F_THEME: usize = 12;
pub const F_RETENTION: usize = 13;
pub const F_START: usize = 14;
pub const F_COUNT: usize = 15;

pub const MODEL_CHOICES: [&str; 5] = ["large-v3-turbo", "large-v3", "medium", "small", "base"];
pub const LANGUAGE_CHOICES: [&str; 2] = ["en", "auto"];
pub const BUFFER_CHOICES: [u32; 6] = [0, 5, 10, 15, 20, 30];
pub const RETENTION_CHOICES: [u32; 5] = [0, 7, 30, 90, 180];
pub const ZOOM_LEVELS: [usize; 4] = [1, 2, 4, 10];

#[derive(Default)]
pub struct TextInput {
    pub value: String,
}

impl TextInput {
    pub fn key(&mut self, k: &KeyEvent) {
        match k.code {
            KeyCode::Char(c) if !k.modifiers.contains(KeyModifiers::CONTROL) => self.value.push(c),
            KeyCode::Backspace => {
                self.value.pop();
            }
            _ => {}
        }
    }
}

pub struct App {
    pub cfg: Config,
    pub theme: Theme,
    pub screen: Screen,
    pub quit: bool,
    pub frame: u64, // spinner / animation counter

    // setup
    pub devices: Vec<String>,
    pub dev_idx: usize,
    pub cursor: usize,
    pub editing: bool,
    pub out_input: TextInput,
    pub name_input: TextInput,
    pub setup_err: String,
    pub setup_note: String,
    pub orphans: Vec<Orphan>,

    // live capture
    pub capture: Option<Capture>, // record mode, or armed meter-only
    pub spool: Option<Spooler>,
    pub sys: Option<SysCapture>,
    pub sys_ready_pending: bool,
    pub sys_level: MeterTick,
    pub mic_file: Option<PathBuf>,
    pub captioner: Option<Captioner>,
    pub captions: Vec<String>,
    pub arm_start: Instant,
    pub mark: Option<Instant>,
    pub file: PathBuf,
    pub name: String,
    pub wave: Vec<WaveCol>,
    pub rmaxdb: f64,
    pub clips: u64,
    pub clip_ticks: u32,
    pub vu_level: f64,
    pub markers: Vec<Duration>,
    pub zoom_idx: usize,
    pub notice: String,
    pub disk_free: Option<u64>,
    pub disk_tick: u32,

    // perf overlay
    pub show_perf: bool,
    pub perf_last: Option<Instant>,
    pub perf_avg_ms: f64,
    pub perf_max_ms: f64,
    pub perf_ticks: f64,
    pub perf_draw_ms: f64,

    // transcription
    pub trans: Option<Transcriber>,
    pub tx_dir: Option<PathBuf>,
    pub trans_log: Vec<String>,
    pub trans_err: Option<String>,
    pub did_trans: bool,
    pub preview: Vec<String>,
    pub show_log: bool,
    pub trans_start: Instant,
    pub stage: usize,
    pub stage_pct: f64,
    pub live_segs: Vec<String>,

    // speakers
    pub segs: Vec<WSeg>,
    pub stats: Vec<SpkStat>,
    pub spk_cursor: usize,
    pub spk_edit: bool,
    pub spk_input: TextInput,

    // done / player
    pub transcript_md: Option<PathBuf>,
    pub markers_file: Option<PathBuf>,
    pub playback: Option<Playback>,
    pub play_gen: u64,
    pub play_start: Instant,
    pub p_pos: f64,
    pub p_dur: f64,
    pub p_wave: Vec<WaveCol>,
    pub p_ready: bool,
    pub decode_rx: Option<Receiver<Result<DecodedWave, String>>>,
    pub post_rx: Option<Receiver<Result<(), String>>>,
    pub post_status: String,
    pub post_ran: bool,

    // library
    pub lib: Vec<LibEntry>,
    pub lib_cursor: usize,
    pub lib_confirm: bool,
    pub from_lib: bool,
    pub lib_searching: bool,
    pub search_input: TextInput,
    pub hits: Vec<SearchHit>,
    pub hit_cursor: usize,
    pub show_hits: bool,

    pub control_rx: Option<Receiver<String>>,
}

impl App {
    pub fn new() -> App {
        let cfg = config::load();
        let theme = apply_overrides(by_name(&cfg.theme), &cfg.theme_colors);
        sweep_stale();
        let mut setup_note = String::new();
        let n = sweep_retention(&cfg.out_dir, cfg.retention_days);
        if n > 0 {
            setup_note = format!(
                "retention: deleted {n} recording(s) older than {} days (transcripts kept)",
                cfg.retention_days
            );
        }
        let devices = crate::audio::list_input_devices();
        let dev_idx = devices.iter().position(|d| *d == cfg.device).unwrap_or(0);
        App {
            out_input: TextInput { value: cfg.out_dir.clone() },
            name_input: TextInput { value: "recording".into() },
            theme,
            screen: Screen::Setup,
            quit: false,
            frame: 0,
            devices,
            dev_idx,
            cursor: 0,
            editing: false,
            setup_err: String::new(),
            setup_note,
            orphans: find_orphans(),
            capture: None,
            spool: None,
            sys: None,
            sys_ready_pending: false,
            sys_level: MeterTick::default(),
            mic_file: None,
            captioner: None,
            captions: Vec::new(),
            arm_start: Instant::now(),
            mark: None,
            file: PathBuf::new(),
            name: "recording".into(),
            wave: Vec::new(),
            rmaxdb: -99.0,
            clips: 0,
            clip_ticks: 0,
            vu_level: 0.0,
            markers: Vec::new(),
            zoom_idx: 1, // 100 ms cells; zoom 0 is the 50 ms close-up
            notice: String::new(),
            disk_free: None,
            disk_tick: 0,
            show_perf: false,
            perf_last: None,
            perf_avg_ms: 0.0,
            perf_max_ms: 0.0,
            perf_ticks: 0.0,
            perf_draw_ms: 0.0,
            trans: None,
            tx_dir: None,
            trans_log: Vec::new(),
            trans_err: None,
            did_trans: false,
            preview: Vec::new(),
            show_log: false,
            trans_start: Instant::now(),
            stage: 0,
            stage_pct: 0.0,
            live_segs: Vec::new(),
            segs: Vec::new(),
            stats: Vec::new(),
            spk_cursor: 0,
            spk_edit: false,
            spk_input: TextInput::default(),
            transcript_md: None,
            markers_file: None,
            playback: None,
            play_gen: 0,
            play_start: Instant::now(),
            p_pos: 0.0,
            p_dur: 0.0,
            p_wave: Vec::new(),
            p_ready: false,
            decode_rx: None,
            post_rx: None,
            post_status: String::new(),
            post_ran: false,
            lib: Vec::new(),
            lib_cursor: 0,
            lib_confirm: false,
            from_lib: false,
            lib_searching: false,
            search_input: TextInput::default(),
            hits: Vec::new(),
            hit_cursor: 0,
            show_hits: false,
            cfg,
            control_rx: control::start_control().ok(),
        }
    }

    pub fn teardown(&mut self) {
        self.stop_playback();
        if let Some(c) = self.captioner.take() {
            c.stop();
        }
        if let Some(s) = self.sys.take() {
            s.stop();
        }
        if let Some(c) = self.capture.take() {
            let _ = c.stop();
        }
        if let Some(sp) = self.spool.take() {
            let dir = sp.dir.clone();
            sp.stop();
            Spooler::cleanup_dir(&dir);
        }
    }

    // ------------------------------------------------------------------
    // tick (40 Hz frame clock)

    pub fn on_tick(&mut self) {
        self.frame += 1;
        self.drain_control();
        match self.screen {
            Screen::Recording | Screen::Armed => self.tick_live(),
            Screen::Transcribing => self.tick_transcribing(),
            Screen::Done => self.tick_done(),
            _ => {}
        }
    }

    fn drain_control(&mut self) {
        let Some(rx) = &self.control_rx else { return };
        let cmds: Vec<String> = rx.try_iter().collect();
        for cmd in cmds {
            match cmd.as_str() {
                "trigger" if self.screen == Screen::Armed => self.armed_trigger(),
                "stop" if self.screen == Screen::Recording => self.stop_and_continue(),
                "marker" if self.screen == Screen::Recording => self.drop_marker(),
                _ => {}
            }
        }
    }

    fn tick_live(&mut self) {
        let now = Instant::now();
        if let Some(last) = self.perf_last {
            let iv = last.elapsed().as_secs_f64() * 1000.0;
            self.perf_avg_ms += (iv - self.perf_avg_ms) * 0.05;
            self.perf_max_ms *= 0.98;
            if iv > self.perf_max_ms {
                self.perf_max_ms = iv;
            }
        }
        self.perf_last = Some(now);

        // system audio readiness + level
        if let Some(sys) = &self.sys {
            if self.sys_ready_pending {
                if let Ok(res) = sys.ready.try_recv() {
                    self.sys_ready_pending = false;
                    if let Err(e) = res {
                        self.notice = format!("system audio unavailable: {e} — recording mic only");
                        if let Some(s) = self.sys.take() {
                            s.stop();
                        }
                    }
                }
            }
        }
        if let Some(sys) = &self.sys {
            for t in sys.ticks.try_iter() {
                self.sys_level = t;
            }
        }

        // capture errors: salvage what we have
        let mut cap_err = None;
        if let Some(cap) = &self.capture {
            if let Ok(e) = cap.errs.try_recv() {
                cap_err = Some(e);
            }
        }
        if let Some(e) = cap_err {
            self.notice = format!("recording stopped: {e}");
            let _ = self.finish_capture();
            self.finish_files();
            self.enter_done();
            return;
        }

        // meter ticks
        let mut pulled = 0u32;
        let mut ticks: Vec<MeterTick> = Vec::new();
        if let Some(cap) = &self.capture {
            ticks.extend(cap.ticks.try_iter());
        }
        for t in ticks {
            self.apply_tick(t);
            pulled += 1;
        }
        self.perf_ticks += (pulled as f64 - self.perf_ticks) * 0.05;

        // captions: pcm through, results out
        let mut pcm: Vec<Vec<i16>> = Vec::new();
        if let Some(cap) = &self.capture {
            pcm.extend(cap.caption_pcm.try_iter());
        }
        if let Some(c) = &self.captioner {
            for p in pcm {
                c.push_pcm(p);
            }
            let new: Vec<String> = c.lines.try_iter().collect();
            for line in new {
                self.captions.push(line);
                if self.captions.len() > 8 {
                    self.captions.remove(0);
                }
            }
        }
    }

    fn apply_tick(&mut self, t: MeterTick) {
        let mut col = WaveCol { rms: t.rms, peak: t.peak, rms_r: t.rms_r, peak_r: t.peak_r, clip: t.clip, paused: false };
        let mut db = t.db;
        if self.sys.is_some() {
            // stereo lanes become mic (top) and system audio (bottom)
            col.rms_r = self.sys_level.rms;
            col.peak_r = self.sys_level.peak;
            col.clip |= self.sys_level.clip;
            db = db.max(self.sys_level.db);
        }
        if self.capture.as_ref().map(|c| c.paused()).unwrap_or(false) {
            col.paused = true;
            col.clip = false;
        }
        self.wave.push(col);
        if self.wave.len() > 16384 {
            self.wave.drain(..8192);
        }
        self.rmaxdb -= 10.0 / TICK_HZ as f64;
        if db > self.rmaxdb {
            self.rmaxdb = db;
        }
        let target = t.peak.max(col.peak_r);
        if target > self.vu_level {
            self.vu_level = target;
        } else {
            self.vu_level *= 0.905;
        }
        if col.clip {
            self.clips += 1;
            self.clip_ticks = 2 * TICK_HZ;
        } else if self.clip_ticks > 0 {
            self.clip_ticks -= 1;
        }
        self.disk_tick += 1;
        if self.disk_tick >= 5 * TICK_HZ {
            self.disk_tick = 0;
            self.disk_free = free_bytes(&self.cfg.out_dir);
        }
    }

    fn tick_transcribing(&mut self) {
        let Some(trans) = &self.trans else { return };
        let lines: Vec<String> = trans.lines.try_iter().collect();
        for line in lines {
            if let Some(s) = stage_of(&line) {
                self.stage = self.stage.max(s);
            }
            if let Some(p) = pct_of(&line) {
                self.stage_pct = p;
            }
            if let Some(text) = seg_text_of(&line) {
                self.live_segs.push(text.to_string());
                if self.live_segs.len() > 4 {
                    self.live_segs.remove(0);
                }
            }
            self.trans_log.push(line);
            if self.trans_log.len() > 400 {
                self.trans_log.drain(..200);
            }
        }
        if let Ok(res) = trans.done.try_recv() {
            let out_dir = trans.out_dir.clone();
            self.trans = None;
            match res {
                Err(e) => {
                    self.trans_err = Some(e);
                    self.did_trans = false;
                    self.enter_done();
                }
                Ok(()) => {
                    self.trans_err = None;
                    self.did_trans = true;
                    self.tx_dir = Some(out_dir.clone());
                    self.preview = transcript_preview(&out_dir, &self.file, 6);
                    self.segs = load_segments(&out_dir, &self.file);
                    self.stats = speaker_stats(&self.segs);
                    if self.segs.is_empty() && self.preview.is_empty() {
                        self.notice = "transcription found no speech — check mic selection and gain (watch the level meter while talking)".into();
                        self.enter_done();
                    } else if self.segs.is_empty() {
                        // transcript text exists but the json defeated us:
                        // never claim silence when there are words on disk
                        self.notice = "transcript ready, but its speaker data could not be parsed".into();
                        self.enter_done();
                    } else if self.stats.len() > 1 {
                        self.spk_cursor = 0;
                        self.spk_edit = false;
                        self.screen = Screen::Speakers;
                    } else {
                        self.enter_done();
                    }
                }
            }
        }
    }

    fn tick_done(&mut self) {
        if let Some(rx) = &self.decode_rx {
            if let Ok(res) = rx.try_recv() {
                self.decode_rx = None;
                if let Ok(dw) = res {
                    if dw.file == self.file {
                        self.p_wave = dw.cols;
                        self.p_dur = dw.dur;
                        self.p_ready = true;
                    }
                }
            }
        }
        let mut ended = false;
        if let Some(pb) = &self.playback {
            if let Ok(gen) = pb.ended.try_recv() {
                ended = gen == self.play_gen;
            }
        }
        if ended {
            if let Some(pb) = self.playback.take() {
                pb.reap();
            }
            self.p_pos += self.play_start.elapsed().as_secs_f64();
            if self.p_dur > 0.0 && self.p_pos >= self.p_dur - 0.5 {
                self.p_pos = 0.0; // natural end: rewind
            }
        }
        if let Some(rx) = &self.post_rx {
            if let Ok(res) = rx.try_recv() {
                self.post_rx = None;
                self.post_status = match res {
                    Ok(()) => "post-hook: done".into(),
                    Err(e) => format!("post-hook failed: {e}"),
                };
            }
        }
    }

    // ------------------------------------------------------------------
    // keys

    /// Returns true when the app should exit.
    pub fn on_key(&mut self, k: KeyEvent) -> bool {
        if k.code == KeyCode::Char('c') && k.modifiers.contains(KeyModifiers::CONTROL) {
            self.teardown();
            return true;
        }
        match self.screen {
            Screen::Setup => self.key_setup(k),
            Screen::Armed => self.key_armed(k),
            Screen::Recording => self.key_recording(k),
            Screen::Transcribing => {
                if k.code == KeyCode::Char('l') {
                    self.show_log = !self.show_log;
                }
            }
            Screen::Speakers => self.key_speakers(k),
            Screen::Library => self.key_library(k),
            Screen::Done => return self.key_done(k),
        }
        self.quit
    }

    fn key_setup(&mut self, k: KeyEvent) {
        if self.editing {
            match k.code {
                KeyCode::Enter | KeyCode::Esc => self.editing = false,
                _ => {
                    if self.cursor == F_OUT_DIR {
                        self.out_input.key(&k);
                    } else {
                        self.name_input.key(&k);
                    }
                }
            }
            return;
        }
        match k.code {
            KeyCode::Char('q') | KeyCode::Esc => self.quit = true,
            KeyCode::Char('b') => {
                self.lib = load_library(&expand_home(&self.out_input.value));
                self.lib_cursor = 0;
                self.lib_confirm = false;
                self.show_hits = false;
                self.lib_searching = false;
                self.screen = Screen::Library;
            }
            KeyCode::Char('r') => {
                if !self.orphans.is_empty() {
                    self.recover_orphan();
                }
            }
            KeyCode::Char('d') => {
                if !self.orphans.is_empty() {
                    let _ = std::fs::remove_dir_all(&self.orphans[0].dir);
                    self.orphans = find_orphans();
                }
            }
            KeyCode::Up | KeyCode::Char('k') => self.cursor = self.cursor.saturating_sub(1),
            KeyCode::Down | KeyCode::Char('j') | KeyCode::Tab => {
                self.cursor = (self.cursor + 1).min(F_COUNT - 1)
            }
            KeyCode::Left | KeyCode::Char('h') => self.adjust_field(-1),
            KeyCode::Right | KeyCode::Char('l') | KeyCode::Char(' ') => self.adjust_field(1),
            KeyCode::Enter => match self.cursor {
                F_OUT_DIR | F_NAME => self.editing = true,
                F_TRANSCRIBE => self.cfg.transcribe = !self.cfg.transcribe,
                F_START => self.start(),
                _ => self.adjust_field(1),
            },
            _ => {}
        }
    }

    fn adjust_field(&mut self, dir: i32) {
        let cyc = |cur: usize, len: usize| -> usize {
            (cur as i32 + dir).rem_euclid(len as i32) as usize
        };
        match self.cursor {
            F_DEVICE => self.dev_idx = cyc(self.dev_idx, self.devices.len().max(1)),
            F_CHANNELS => self.cfg.channels = if self.cfg.channels == 1 { 2 } else { 1 },
            F_SYS_AUDIO => self.cfg.system_audio = !self.cfg.system_audio,
            F_MODE => self.cfg.mode = if self.cfg.mode == "record" { "armed".into() } else { "record".into() },
            F_BUFFER => {
                let i = BUFFER_CHOICES.iter().position(|&b| b == self.cfg.buffer_min).unwrap_or(0);
                self.cfg.buffer_min = BUFFER_CHOICES[cyc(i, BUFFER_CHOICES.len())];
            }
            F_TRANSCRIBE => self.cfg.transcribe = !self.cfg.transcribe,
            F_CAPTIONS => self.cfg.live_captions = !self.cfg.live_captions,
            F_MODEL => {
                let i = MODEL_CHOICES.iter().position(|&m| m == self.cfg.model).unwrap_or(0);
                self.cfg.model = MODEL_CHOICES[cyc(i, MODEL_CHOICES.len())].into();
            }
            F_SPEAKERS => {
                let v = self.cfg.speakers as i32 + dir;
                self.cfg.speakers = if v < 0 { 8 } else if v > 8 { 0 } else { v as u32 };
            }
            F_LANGUAGE => {
                let i = LANGUAGE_CHOICES.iter().position(|&l| l == self.cfg.language).unwrap_or(0);
                self.cfg.language = LANGUAGE_CHOICES[cyc(i, LANGUAGE_CHOICES.len())].into();
            }
            F_THEME => {
                let names = theme_names();
                let i = names.iter().position(|&n| n == self.cfg.theme).unwrap_or(0);
                self.cfg.theme = names[cyc(i, names.len())].into();
                self.theme = apply_overrides(by_name(&self.cfg.theme), &self.cfg.theme_colors);
                let _ = self.cfg.save();
            }
            F_RETENTION => {
                let i = RETENTION_CHOICES.iter().position(|&r| r == self.cfg.retention_days).unwrap_or(0);
                self.cfg.retention_days = RETENTION_CHOICES[cyc(i, RETENTION_CHOICES.len())];
            }
            _ => {}
        }
    }

    fn key_armed(&mut self, k: KeyEvent) {
        match k.code {
            KeyCode::Char('f') => self.show_perf = !self.show_perf,
            KeyCode::Enter | KeyCode::Char('s') => self.armed_trigger(),
            KeyCode::Char('x') | KeyCode::Esc => {
                // disarm: nothing survives unless triggered
                self.teardown();
                self.screen = Screen::Setup;
            }
            _ => {}
        }
    }

    fn armed_trigger(&mut self) {
        if self.spool.is_none() {
            // no buffer: standby only — recording starts right now
            if let Some(c) = self.capture.take() {
                let _ = c.stop();
            }
            self.start_recording();
            return;
        }
        self.mark = Some(Instant::now());
        if let Some(sp) = &self.spool {
            sp.trigger();
        }
        self.file = PathBuf::from(&self.cfg.out_dir).join(format!(
            "{}-{}.flac",
            self.name,
            chrono::Local::now().format("%Y%m%d-%H%M%S")
        ));
        self.screen = Screen::Recording;
    }

    fn key_recording(&mut self, k: KeyEvent) {
        match k.code {
            KeyCode::Char('f') => self.show_perf = !self.show_perf,
            KeyCode::Enter | KeyCode::Char('s') => self.stop_and_continue(),
            KeyCode::Char('m') => self.drop_marker(),
            KeyCode::Char('+') | KeyCode::Char('=') => self.zoom_idx = self.zoom_idx.saturating_sub(1),
            KeyCode::Char('-') | KeyCode::Char('_') => {
                self.zoom_idx = (self.zoom_idx + 1).min(ZOOM_LEVELS.len() - 1)
            }
            KeyCode::Char('p') | KeyCode::Char(' ') => {
                if self.sys.is_some() {
                    self.notice = "pause is not available while capturing system audio".into();
                } else if let Some(cap) = &self.capture {
                    if cap.paused() {
                        cap.resume();
                    } else {
                        cap.pause();
                    }
                }
            }
            KeyCode::Char('x') | KeyCode::Esc => {
                // keep the file, skip transcription
                if let Err(e) = self.finish_capture() {
                    self.trans_err = Some(e);
                }
                self.finish_files();
                self.enter_done();
            }
            _ => {}
        }
    }

    fn drop_marker(&mut self) {
        if self.capture.as_ref().map(|c| c.paused()).unwrap_or(false) {
            return;
        }
        self.markers.push(self.live_elapsed());
    }

    fn stop_and_continue(&mut self) {
        if let Err(e) = self.finish_capture() {
            self.trans_err = Some(e);
            self.enter_done();
            return;
        }
        self.finish_files();
        if self.cfg.transcribe {
            self.begin_transcribe();
        } else {
            self.enter_done();
        }
    }

    pub fn live_elapsed(&self) -> Duration {
        if self.spool.is_some() {
            return self.mark.map(|m| m.elapsed()).unwrap_or_default();
        }
        self.capture.as_ref().map(|c| c.elapsed()).unwrap_or_default()
    }

    // ------------------------------------------------------------------
    // capture lifecycle

    fn start(&mut self) {
        self.setup_err.clear();
        self.cfg.device = self.devices.get(self.dev_idx).cloned().unwrap_or_else(|| DEFAULT_DEVICE.into());
        self.cfg.out_dir = expand_home(&self.out_input.value);
        self.name = self.name_input.value.trim().to_string();
        if self.name.is_empty() {
            self.name = "recording".into();
        }
        if let Err(e) = std::fs::create_dir_all(&self.cfg.out_dir) {
            self.setup_err = e.to_string();
            return;
        }
        let _ = self.cfg.save();

        self.wave.clear();
        self.rmaxdb = -99.0;
        self.clips = 0;
        self.clip_ticks = 0;
        self.vu_level = 0.0;
        self.markers.clear();
        self.notice.clear();
        self.post_status.clear();
        self.post_ran = false;
        self.from_lib = false;
        self.captions.clear();
        self.disk_free = free_bytes(&self.cfg.out_dir);

        if self.cfg.mode == "armed" {
            self.start_armed();
        } else {
            self.start_recording();
        }
    }

    fn start_recording(&mut self) {
        let ts = chrono::Local::now().format("%Y%m%d-%H%M%S").to_string();
        self.file = PathBuf::from(&self.cfg.out_dir).join(format!("{}-{ts}.flac", self.name));

        let use_sys = self.cfg.system_audio && self.cfg.mode == "record";
        let mut target = self.file.clone();
        let mut channels = self.cfg.channels;
        if use_sys {
            // mic to a temp mono track, merged with the system track at stop
            let _ = std::fs::create_dir_all(sessions_dir());
            let mic = sessions_dir().join(format!("mic-{ts}.flac"));
            self.mic_file = Some(mic.clone());
            target = mic;
            channels = 1;
        } else {
            self.mic_file = None;
        }

        match Capture::start(
            &self.cfg.device,
            CaptureCfg { encode_to: Some(target), target_channels: channels, captions: self.cfg.live_captions },
        ) {
            Ok(c) => self.capture = Some(c),
            Err(e) => {
                self.setup_err = e;
                self.screen = Screen::Setup;
                return;
            }
        }
        self.screen = Screen::Recording;

        if self.cfg.live_captions {
            match start_captioner() {
                Ok(c) => self.captioner = Some(c),
                Err(e) => self.notice = format!("live captions unavailable: {e}"),
            }
        }
        if use_sys {
            let sys_file = sessions_dir().join(format!("sys-{ts}.flac"));
            match start_sys_capture(sys_file) {
                Ok(s) => {
                    self.sys = Some(s);
                    self.sys_ready_pending = true;
                }
                Err(e) => {
                    self.notice = format!("system audio unavailable: {e} — recording mic only");
                }
            }
        }
    }

    fn start_armed(&mut self) {
        let mut spool = None;
        if self.cfg.buffer_min > 0 {
            let window = Duration::from_secs(self.cfg.buffer_min as u64 * 60);
            let idx = if self.cfg.device == DEFAULT_DEVICE { None } else { av_index_for(&self.cfg.device) };
            match start_spooler(idx, self.cfg.channels, window) {
                Ok(s) => spool = Some(s),
                Err(e) => {
                    self.setup_err = e;
                    return;
                }
            }
        }
        // meter-only capture for the live display
        match Capture::start(
            &self.cfg.device,
            CaptureCfg { encode_to: None, target_channels: self.cfg.channels, captions: false },
        ) {
            Ok(c) => self.capture = Some(c),
            Err(e) => {
                if let Some(sp) = spool.take() {
                    let dir = sp.dir.clone();
                    sp.stop();
                    Spooler::cleanup_dir(&dir);
                }
                self.setup_err = e;
                return;
            }
        }
        self.spool = spool;
        self.arm_start = Instant::now();
        self.mark = None;
        self.screen = Screen::Armed;
    }

    /// Stop whichever capture path is live and assemble self.file.
    fn finish_capture(&mut self) -> Result<(), String> {
        if let Some(c) = self.captioner.take() {
            c.stop();
        }
        if let Some(sp) = self.spool.take() {
            // armed: buffer + live tail
            if let Some(c) = self.capture.take() {
                let _ = c.stop(); // meter-only
            }
            let dir = sp.dir.clone();
            let segs = sp.stop();
            let res = crate::state::concat_flac(&segs, &self.file);
            Spooler::cleanup_dir(&dir);
            return res;
        }
        let Some(cap) = self.capture.take() else {
            return Err("nothing was recording".into());
        };
        let res = cap.stop();
        if let Some(sys) = self.sys.take() {
            let sys_file = sys.file.clone();
            sys.stop();
            let mic = self.mic_file.take().ok_or("mic track missing")?;
            res?;
            let merged = merge_mic_system(&mic, &sys_file, &self.file);
            let _ = std::fs::remove_file(&mic);
            let _ = std::fs::remove_file(&sys_file);
            return merged;
        }
        if let Some(mic) = self.mic_file.take() {
            // system audio fell over mid-flight; keep the mic track
            res?;
            return std::fs::rename(&mic, &self.file).map_err(|e| e.to_string());
        }
        res
    }

    fn finish_files(&mut self) {
        self.markers_file = write_markers_file(&self.file, &self.markers);
    }

    fn begin_transcribe(&mut self) {
        match start_transcribe(&self.file, &self.cfg.model, &self.cfg.language, self.cfg.speakers) {
            Ok(t) => {
                self.trans = Some(t);
                self.trans_log.clear();
                self.live_segs.clear();
                self.stage = 0;
                self.stage_pct = 0.0;
                self.show_log = false;
                self.post_ran = false; // a fresh transcription earns a fresh post-hook
                self.trans_start = Instant::now();
                self.screen = Screen::Transcribing;
            }
            Err(e) => {
                self.trans_err = Some(e);
                self.enter_done();
            }
        }
    }

    fn recover_orphan(&mut self) {
        let o = &self.orphans[0];
        let file = if o.meta.file.is_empty() {
            PathBuf::from(expand_home(&self.out_input.value)).join(format!(
                "recovered-{}.flac",
                chrono::Local::now().format("%Y%m%d-%H%M%S")
            ))
        } else {
            PathBuf::from(&o.meta.file)
        };
        if let Some(dir) = file.parent() {
            let _ = std::fs::create_dir_all(dir);
        }
        match crate::state::concat_flac(&o.segments, &file) {
            Err(e) => self.setup_err = format!("recovery failed: {e}"),
            Ok(()) => {
                let _ = std::fs::remove_dir_all(&o.dir);
                self.orphans = find_orphans();
                self.file = file;
                self.did_trans = false;
                self.trans_err = None;
                self.notice = "recovered from unfinished session".into();
                self.markers.clear();
                self.enter_done();
            }
        }
    }

    // ------------------------------------------------------------------
    // speakers

    fn key_speakers(&mut self, k: KeyEvent) {
        if self.spk_edit {
            match k.code {
                KeyCode::Enter => {
                    self.stats[self.spk_cursor].name = self.spk_input.value.trim().to_string();
                    self.spk_edit = false;
                }
                KeyCode::Esc => self.spk_edit = false,
                _ => self.spk_input.key(&k),
            }
            return;
        }
        match k.code {
            KeyCode::Up | KeyCode::Char('k') => self.spk_cursor = self.spk_cursor.saturating_sub(1),
            KeyCode::Down | KeyCode::Char('j') | KeyCode::Tab => {
                self.spk_cursor = (self.spk_cursor + 1).min(self.stats.len().saturating_sub(1))
            }
            KeyCode::Enter => {
                self.spk_input.value = self.stats[self.spk_cursor].name.clone();
                self.spk_edit = true;
            }
            KeyCode::Char('c') | KeyCode::Char('s') => self.apply_speakers(),
            _ => {}
        }
    }

    fn apply_speakers(&mut self) {
        if let Some(dir) = &self.tx_dir {
            if let Ok(path) = write_named_transcript(dir, &self.file, &self.segs, &self.stats, &self.markers) {
                self.transcript_md = Some(path);
            }
        }
        self.enter_done();
    }

    fn enter_done(&mut self) {
        if self.did_trans && self.transcript_md.is_none() && !self.segs.is_empty() {
            if let Some(dir) = &self.tx_dir {
                if let Ok(path) = write_named_transcript(dir, &self.file, &self.segs, &self.stats, &self.markers) {
                    self.transcript_md = Some(path);
                }
            }
        }
        self.screen = Screen::Done;
        self.reset_player();
        self.ensure_decode();
        if self.did_trans && !self.post_ran && !self.cfg.post_command.is_empty() {
            self.post_ran = true;
            self.post_status = "post-hook: running...".into();
            self.post_rx = Some(run_post_hook(
                &self.cfg.post_command,
                &self.file,
                self.tx_dir.as_deref(),
                self.transcript_md.as_deref(),
                self.markers_file.as_deref(),
            ));
        }
    }

    // ------------------------------------------------------------------
    // player

    fn reset_player(&mut self) {
        self.stop_playback();
        self.p_wave.clear();
        self.p_dur = 0.0;
        self.p_pos = 0.0;
        self.p_ready = false;
        self.decode_rx = None;
    }

    fn ensure_decode(&mut self) {
        if self.p_ready || self.decode_rx.is_some() || !self.file.exists() {
            return;
        }
        self.decode_rx = Some(decode_wave(self.file.clone(), 240 * 2));
    }

    pub fn cur_pos(&self) -> f64 {
        let mut pos = self.p_pos;
        if self.playback.is_some() {
            pos += self.play_start.elapsed().as_secs_f64();
        }
        if self.p_dur > 0.0 {
            pos = pos.min(self.p_dur);
        }
        pos.max(0.0)
    }

    fn stop_playback(&mut self) {
        if let Some(pb) = self.playback.take() {
            self.p_pos += self.play_start.elapsed().as_secs_f64();
            if self.p_dur > 0.0 {
                self.p_pos = self.p_pos.min(self.p_dur);
            }
            self.play_gen += 1; // orphan the watcher
            pb.stop();
        }
    }

    fn begin_playback(&mut self) {
        self.play_gen += 1;
        match start_playback(&self.file, self.p_pos, self.play_gen) {
            Ok(pb) => {
                self.playback = Some(pb);
                self.play_start = Instant::now();
            }
            Err(e) => self.notice = e,
        }
    }

    fn seek_to(&mut self, t: f64) {
        let was_playing = self.playback.is_some();
        self.stop_playback();
        let mut t = t.max(0.0);
        if self.p_dur > 0.0 {
            t = t.min((self.p_dur - 0.2).max(0.0));
        }
        self.p_pos = t;
        if was_playing {
            self.begin_playback();
        }
    }

    // ------------------------------------------------------------------
    // done + library keys

    fn key_done(&mut self, k: KeyEvent) -> bool {
        match k.code {
            KeyCode::Char('q') => {
                self.teardown();
                return true;
            }
            KeyCode::Esc => {
                if self.from_lib {
                    self.stop_playback();
                    self.lib = load_library(&self.cfg.out_dir);
                    self.screen = Screen::Library;
                    return false;
                }
                self.teardown();
                return true;
            }
            KeyCode::Char('o') => {
                if let Some(dir) = self.file.parent() {
                    let _ = std::process::Command::new("open").arg(dir).spawn();
                }
            }
            KeyCode::Char('p') | KeyCode::Char(' ') => {
                if self.playback.is_some() {
                    self.stop_playback();
                } else {
                    self.begin_playback();
                    self.ensure_decode();
                }
            }
            KeyCode::Left | KeyCode::Char('h') => self.seek_to(self.cur_pos() - 5.0),
            KeyCode::Right | KeyCode::Char('l') => self.seek_to(self.cur_pos() + 5.0),
            KeyCode::Char('[') => {
                let cur = self.cur_pos();
                let target = self
                    .markers
                    .iter()
                    .map(|m| m.as_secs_f64())
                    .filter(|&s| s < cur - 1.0)
                    .fold(0.0, f64::max);
                self.seek_to(target);
            }
            KeyCode::Char(']') => {
                let cur = self.cur_pos();
                if let Some(s) = self
                    .markers
                    .iter()
                    .map(|m| m.as_secs_f64())
                    .find(|&s| s > cur + 0.5)
                {
                    self.seek_to(s);
                }
            }
            KeyCode::Char('t') => {
                if self.file.exists() {
                    self.trans_err = None;
                    self.begin_transcribe();
                }
            }
            KeyCode::Char('s') => {
                if self.did_trans && !self.stats.is_empty() {
                    self.spk_cursor = 0;
                    self.spk_edit = false;
                    self.screen = Screen::Speakers;
                }
            }
            KeyCode::Char('n') => {
                self.stop_playback();
                let mut fresh = App::new();
                fresh.control_rx = self.control_rx.take();
                *self = fresh;
            }
            _ => {}
        }
        false
    }

    fn open_lib_entry(&mut self, e: &LibEntry) {
        self.file = e.path.clone();
        self.did_trans = e.has_tx;
        self.trans_err = None;
        self.transcript_md = None;
        self.markers_file = None;
        self.post_ran = true; // never auto-run hooks on old recordings
        self.post_status.clear();
        self.notice.clear();
        self.from_lib = true;
        self.reset_player();
        self.markers = load_markers_file(&e.path);
        if e.has_tx {
            let dir = tx_dir_for(&e.path);
            self.preview = transcript_preview(&dir, &e.path, 6);
            self.segs = load_segments(&dir, &e.path);
            self.stats = speaker_stats(&self.segs);
            let md = dir.join(format!("{}-transcript.md", crate::transcribe::audio_stem(&e.path)));
            if md.exists() {
                self.transcript_md = Some(md);
            }
            self.tx_dir = Some(dir);
        } else {
            self.tx_dir = None;
            self.preview.clear();
            self.segs.clear();
            self.stats.clear();
        }
        self.screen = Screen::Done;
        self.ensure_decode();
    }

    fn key_library(&mut self, k: KeyEvent) {
        if self.lib_searching {
            match k.code {
                KeyCode::Esc => self.lib_searching = false,
                KeyCode::Enter => {
                    self.lib_searching = false;
                    self.hits = search_transcripts(&self.lib, &self.search_input.value);
                    self.hit_cursor = 0;
                    self.show_hits = true;
                }
                _ => self.search_input.key(&k),
            }
            return;
        }
        if self.show_hits {
            match k.code {
                KeyCode::Esc | KeyCode::Char('q') => self.show_hits = false,
                KeyCode::Up | KeyCode::Char('k') => self.hit_cursor = self.hit_cursor.saturating_sub(1),
                KeyCode::Down | KeyCode::Char('j') => {
                    self.hit_cursor = (self.hit_cursor + 1).min(self.hits.len().saturating_sub(1))
                }
                KeyCode::Char('/') => {
                    self.lib_searching = true;
                    self.search_input.value.clear();
                }
                KeyCode::Enter if !self.hits.is_empty() => {
                    let h = self.hits[self.hit_cursor].clone();
                    if let Some(e) = self.lib.iter().find(|e| e.path == h.audio).cloned() {
                        self.open_lib_entry(&e);
                        let ctx = hit_context(&h, 2);
                        if !ctx.is_empty() {
                            self.preview = ctx;
                        }
                    }
                }
                _ => {}
            }
            return;
        }
        if self.lib_confirm {
            if k.code == KeyCode::Char('y') {
                if let Some(e) = self.lib.get(self.lib_cursor) {
                    delete_recording(&e.path);
                }
                self.lib = load_library(&self.cfg.out_dir);
                self.lib_cursor = self.lib_cursor.min(self.lib.len().saturating_sub(1));
            }
            self.lib_confirm = false;
            return;
        }
        match k.code {
            KeyCode::Esc | KeyCode::Char('q') | KeyCode::Char('b') => self.screen = Screen::Setup,
            KeyCode::Char('/') => {
                self.lib_searching = true;
                self.search_input.value.clear();
            }
            KeyCode::Up | KeyCode::Char('k') => self.lib_cursor = self.lib_cursor.saturating_sub(1),
            KeyCode::Down | KeyCode::Char('j') => {
                self.lib_cursor = (self.lib_cursor + 1).min(self.lib.len().saturating_sub(1))
            }
            KeyCode::Char('d') => {
                if !self.lib.is_empty() {
                    self.lib_confirm = true;
                }
            }
            KeyCode::Char('o') => {
                let _ = std::process::Command::new("open").arg(&self.cfg.out_dir).spawn();
            }
            KeyCode::Enter => {
                if let Some(e) = self.lib.get(self.lib_cursor).cloned() {
                    self.open_lib_entry(&e);
                }
            }
            _ => {}
        }
    }
}

pub fn expand_home(p: &str) -> String {
    let p = p.trim();
    if let Some(rest) = p.strip_prefix("~/") {
        if let Some(home) = dirs::home_dir() {
            return home.join(rest).to_string_lossy().into_owned();
        }
    }
    p.to_string()
}

pub fn transcript_preview(out_dir: &std::path::Path, audio: &std::path::Path, lines: usize) -> Vec<String> {
    let path = out_dir.join(format!("{}.txt", crate::transcribe::audio_stem(audio)));
    let Ok(data) = std::fs::read_to_string(path) else {
        return Vec::new();
    };
    data.lines()
        .map(str::trim)
        .filter(|l| !l.is_empty())
        .take(lines)
        .map(String::from)
        .collect()
}

fn run_post_hook(
    cmd: &str,
    audio: &std::path::Path,
    tx_dir: Option<&std::path::Path>,
    transcript_md: Option<&std::path::Path>,
    markers: Option<&std::path::Path>,
) -> Receiver<Result<(), String>> {
    let (tx, rx) = crossbeam_channel::bounded(1);
    let cmd = cmd.to_string();
    let audio = audio.to_path_buf();
    let tx_dir = tx_dir.map(|p| p.to_path_buf()).unwrap_or_default();
    let md = transcript_md.map(|p| p.to_path_buf()).unwrap_or_default();
    let mk = markers.map(|p| p.to_path_buf()).unwrap_or_default();
    std::thread::spawn(move || {
        let out = std::process::Command::new("sh")
            .args(["-c", &cmd])
            .env("SB_AUDIO", &audio)
            .env("SB_TRANSCRIPT_DIR", &tx_dir)
            .env("SB_TRANSCRIPT_MD", &md)
            .env("SB_MARKERS", &mk)
            .output();
        let res = match out {
            Ok(o) if o.status.success() => Ok(()),
            Ok(o) => {
                let tail: String = String::from_utf8_lossy(&o.stderr).chars().rev().take(200).collect::<String>().chars().rev().collect();
                Err(format!("{}: {}", o.status, tail.trim()))
            }
            Err(e) => Err(e.to_string()),
        };
        let _ = tx.send(res);
    });
    rx
}
