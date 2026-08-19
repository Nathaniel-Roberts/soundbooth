use crossbeam_channel::{bounded, Receiver};
use std::io::{BufRead, BufReader, Write};
use std::os::unix::net::{UnixListener, UnixStream};
use std::path::PathBuf;

use crate::state::state_dir;

pub fn control_sock_path() -> PathBuf {
    state_dir().join("control.sock")
}

/// Listen on the unix control socket; one-line commands from
/// `soundbooth trigger|stop|marker` arrive on the returned channel so a
/// Stream Deck key or shell alias can drive the TUI.
pub fn start_control() -> std::io::Result<Receiver<String>> {
    let path = control_sock_path();
    std::fs::create_dir_all(state_dir())?;
    // if another instance is alive on the socket, leave it alone — a dev
    // build must never steal trigger/stop from a recording in progress
    if UnixStream::connect(&path).is_ok() {
        return Err(std::io::Error::new(
            std::io::ErrorKind::AddrInUse,
            "another soundbooth instance owns the control socket",
        ));
    }
    let _ = std::fs::remove_file(&path);
    let listener = UnixListener::bind(&path)?;
    let (tx, rx) = bounded::<String>(16);
    std::thread::spawn(move || {
        for stream in listener.incoming().flatten() {
            let tx = tx.clone();
            std::thread::spawn(move || {
                let mut stream = stream;
                let mut line = String::new();
                let mut reader = BufReader::new(stream.try_clone().unwrap());
                if reader.read_line(&mut line).is_ok() {
                    let cmd = line.trim().to_string();
                    if !cmd.is_empty() {
                        let _ = tx.try_send(cmd);
                    }
                }
                let _ = stream.write_all(b"ok\n");
            });
        }
    });
    Ok(rx)
}

/// Deliver a command to a running soundbooth instance.
pub fn send_control(cmd: &str) -> Result<(), String> {
    let mut stream = UnixStream::connect(control_sock_path())
        .map_err(|e| format!("no running soundbooth instance ({e})"))?;
    stream.write_all(format!("{cmd}\n").as_bytes()).map_err(|e| e.to_string())?;
    let mut buf = [0u8; 16];
    use std::io::Read;
    let _ = stream.read(&mut buf);
    Ok(())
}

pub fn cleanup_socket() {
    let _ = std::fs::remove_file(control_sock_path());
}
