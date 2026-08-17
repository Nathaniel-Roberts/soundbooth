mod app;
mod audio;
mod captions;
mod config;
mod control;
mod player;
mod search;
mod speakers;
mod spool;
mod state;
mod sysaudio;
mod theme;
mod transcribe;
mod views;
mod waveform;

use crossterm::event::{self, Event, KeyEventKind};
use crossterm::terminal::{disable_raw_mode, enable_raw_mode, EnterAlternateScreen, LeaveAlternateScreen};
use ratatui::backend::CrosstermBackend;
use ratatui::Terminal;
use std::io::stdout;
use std::time::{Duration, Instant};

use crate::audio::TICK_HZ;

fn main() {
    let args: Vec<String> = std::env::args().collect();
    if let Some(cmd) = args.get(1).map(String::as_str) {
        match cmd {
            "trigger" | "stop" | "marker" => {
                match control::send_control(cmd) {
                    Ok(()) => println!("ok"),
                    Err(e) => {
                        eprintln!("error: {e}");
                        std::process::exit(1);
                    }
                }
                return;
            }
            "doctor" => std::process::exit(doctor()),
            "devices" | "--devices" => {
                for d in audio::list_input_devices() {
                    println!("{d}");
                }
                return;
            }
            other => {
                eprintln!("unknown command: {other} (try: doctor, devices, trigger, marker, stop)");
                std::process::exit(2);
            }
        }
    }

    if let Err(e) = run_tui() {
        // best-effort terminal restore happens in run_tui's cleanup
        eprintln!("error: {e}");
        std::process::exit(1);
    }
}

fn run_tui() -> std::io::Result<()> {
    enable_raw_mode()?;
    crossterm::execute!(stdout(), EnterAlternateScreen)?;
    let mut terminal = Terminal::new(CrosstermBackend::new(stdout()))?;
    let result = event_loop(&mut terminal);
    disable_raw_mode()?;
    crossterm::execute!(stdout(), LeaveAlternateScreen)?;
    terminal.show_cursor()?;
    control::cleanup_socket();
    result
}

fn event_loop(terminal: &mut Terminal<CrosstermBackend<std::io::Stdout>>) -> std::io::Result<()> {
    let mut app = app::App::new();
    let tick = Duration::from_millis(1000 / TICK_HZ as u64);
    let mut next = Instant::now() + tick;
    loop {
        let timeout = next.saturating_duration_since(Instant::now());
        if event::poll(timeout)? {
            if let Event::Key(k) = event::read()? {
                if k.kind == KeyEventKind::Press && app.on_key(k) {
                    return Ok(());
                }
            }
        }
        let now = Instant::now();
        if now >= next {
            next += tick;
            if now > next {
                next = now + tick; // don't spiral after a stall
            }
            app.on_tick();
            if app.quit {
                app.teardown();
                return Ok(());
            }
            let draw_start = Instant::now();
            terminal.draw(|f| views::draw(f, &mut app))?;
            let d = draw_start.elapsed().as_secs_f64() * 1000.0;
            app.perf_draw_ms += (d - app.perf_draw_ms) * 0.05;
        }
    }
}

fn doctor() -> i32 {
    let mut fails = 0;
    let mut check = |label: &str, ok: bool, fix: &str| {
        let mark = if ok { "ok " } else { "FAIL" };
        if !ok {
            fails += 1;
        }
        if ok || fix.is_empty() {
            println!("  [{mark}] {label}");
        } else {
            println!("  [{mark}] {label} — {fix}");
        }
    };
    println!("soundbooth doctor");
    check("sox (encode + playback)", state::find_bin("sox").is_some(), "brew install sox");
    check(
        "ffmpeg (armed-mode buffer)",
        state::find_bin("ffmpeg").is_some(),
        "brew install ffmpeg",
    );
    check(
        "whispermlx (transcription)",
        state::find_bin("whispermlx").is_some(),
        "uv tool install --python 3.13 --with 'numba>=0.61' whispermlx",
    );
    check(
        "xcrun/swiftc (system audio helper)",
        state::find_bin("xcrun").is_some(),
        "xcode-select --install",
    );
    check(
        "Hugging Face token (diarisation)",
        config::hf_token_present(),
        "accept pyannote terms, save a read token to ~/.cache/huggingface/token",
    );
    let home = dirs::home_dir().unwrap_or_default();
    let rec = home.join("Recordings");
    let free = state::free_bytes(&rec.to_string_lossy()).or_else(|| state::free_bytes(&home.to_string_lossy()));
    match free {
        Some(f) => check(&format!("disk space ({} free)", state::human_size(f)), f > 2 << 30, "free up some space"),
        None => check("disk space", false, "could not stat"),
    }
    let devices = audio::list_input_devices();
    check(
        &format!("input devices ({} found)", devices.len().saturating_sub(1)),
        devices.len() > 1,
        "check microphone permissions in System Settings",
    );
    if fails == 0 {
        println!("all good");
        0
    } else {
        println!("{fails} problem(s)");
        1
    }
}

#[cfg(test)]
mod tests;
