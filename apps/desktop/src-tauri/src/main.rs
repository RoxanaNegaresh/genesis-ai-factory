// Genesis AI Factory — desktop shell.
//
// The shell owns the lifecycle of the Go control plane: it starts it as a child
// process on a loopback port, waits for it to become healthy, reads the session
// token it writes, and guarantees it is terminated when the window closes.
// Running the engine as a supervised child rather than a system service is what
// makes the app a single self-contained install with no daemons left behind.

#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

use std::process::{Child, Command, Stdio};
use std::sync::Mutex;
use std::time::{Duration, Instant};

use serde::{Deserialize, Serialize};
use tauri::{Manager, State};

const API_BASE: &str = "http://127.0.0.1:8787";
const HEALTH_TIMEOUT: Duration = Duration::from_secs(20);

/// Handle to the supervised control-plane process.
struct Engine(Mutex<Option<Child>>);

#[derive(Debug, Serialize, Deserialize, Clone)]
struct SessionFile {
    access_token: String,
    #[serde(default)]
    refresh_token: String,
    #[serde(default)]
    email: String,
    #[serde(default)]
    user_id: String,
}

#[derive(Debug, Serialize)]
struct EngineStatus {
    running: bool,
    api_base: String,
    token: Option<String>,
    // The refresh token travels with the access token. Without it the UI
    // cannot renew, and a 15-minute access token means the app stops working
    // a quarter of an hour after launch.
    refresh_token: Option<String>,
    message: String,
}

/// Locates the engine binary.
///
/// The engine ships inside the application bundle as a Tauri sidecar, so a
/// packaged install is self-contained: there is nothing to install separately,
/// no PATH to configure and no daemon left running afterwards. Tauri renames a
/// sidecar to include the target triple when bundling and strips it again on
/// extraction, so both spellings are searched.
///
/// The development fallbacks exist because `cargo tauri dev` runs the binary
/// straight out of target/debug, where no bundle layout exists.
fn server_binary() -> String {
    let names: [&str; 2] = if cfg!(windows) {
        ["genesis-server.exe", "genesis-server"]
    } else {
        ["genesis-server", "genesis-server.exe"]
    };

    if let Ok(exe) = std::env::current_exe() {
        if let Some(dir) = exe.parent() {
            // Beside the executable — how a bundled sidecar is laid out on
            // Windows and Linux.
            for candidate in names {
                let path = dir.join(candidate);
                if path.exists() {
                    return path.to_string_lossy().into_owned();
                }
            }

            // macOS .app: Contents/MacOS/<exe> with resources one level up.
            for relative in ["../Resources", "../lib", "binaries"] {
                for candidate in names {
                    let path = dir.join(relative).join(candidate);
                    if path.exists() {
                        return path.to_string_lossy().into_owned();
                    }
                }
            }

            // Development: repository bin/ directory, found by walking up.
            let mut probe = dir.to_path_buf();
            for _ in 0..6 {
                for candidate in names {
                    let path = probe.join("bin").join(candidate);
                    if path.exists() {
                        return path.to_string_lossy().into_owned();
                    }
                }
                if !probe.pop() {
                    break;
                }
            }
        }
    }

    // Last resort: whatever the PATH offers.
    names[0].to_string()
}

fn data_dir() -> std::path::PathBuf {
    dirs_config().join("genesis")
}

fn dirs_config() -> std::path::PathBuf {
    #[cfg(target_os = "windows")]
    {
        if let Ok(appdata) = std::env::var("APPDATA") {
            return std::path::PathBuf::from(appdata);
        }
    }
    #[cfg(not(target_os = "windows"))]
    {
        if let Ok(xdg) = std::env::var("XDG_CONFIG_HOME") {
            return std::path::PathBuf::from(xdg);
        }
        if let Ok(home) = std::env::var("HOME") {
            return std::path::PathBuf::from(home).join(".config");
        }
    }
    std::env::temp_dir()
}

/// Polls the health endpoint until the engine answers or the timeout elapses.
///
/// Waiting for readiness rather than sleeping a fixed interval is what prevents
/// the window from loading against a server that has not finished migrating.
fn wait_for_health() -> bool {
    let deadline = Instant::now() + HEALTH_TIMEOUT;
    while Instant::now() < deadline {
        if let Ok(response) = ureq::get(&format!("{API_BASE}/health"))
            .timeout(Duration::from_millis(700))
            .call()
        {
            if response.status() == 200 {
                return true;
            }
        }
        std::thread::sleep(Duration::from_millis(200));
    }
    false
}

fn read_session() -> Option<SessionFile> {
    let path = data_dir().join("session.json");
    let raw = std::fs::read_to_string(path).ok()?;
    serde_json::from_str(&raw).ok()
}

/// Starts the engine unless one is already listening.
fn start_engine(engine: &State<Engine>) -> EngineStatus {
    // An already-healthy server (started manually, or left by a previous
    // instance) is adopted rather than duplicated: two servers on one port
    // would fail confusingly.
    if wait_for_health_once() {
        return EngineStatus {
            running: true,
            api_base: API_BASE.to_string(),
            token: read_session().map(|s| s.access_token),
            refresh_token: read_session().map(|s| s.refresh_token),
            message: "Attached to a running engine".into(),
        };
    }

    let binary = server_binary();

    // The engine's output goes to a log file rather than to /dev/null.
    //
    // Discarding it makes every startup failure look identical from the UI —
    // "the engine started but never became healthy" — whether the binary is
    // missing, not executable, refused the port, or crashed on a bad
    // configuration. That is the difference between a diagnosable problem and
    // a support ticket. If the log cannot be opened the process still starts;
    // losing the log is worse than nothing, but not worth refusing to run.
    let log_path = data_dir().join("engine.log");
    let _ = std::fs::create_dir_all(data_dir());
    let (out, err) = match std::fs::File::create(&log_path) {
        Ok(file) => match file.try_clone() {
            Ok(clone) => (Stdio::from(file), Stdio::from(clone)),
            Err(_) => (Stdio::null(), Stdio::null()),
        },
        Err(_) => (Stdio::null(), Stdio::null()),
    };

    let child = Command::new(&binary)
        .env("GENESIS_ADDR", "127.0.0.1:8787")
        .env("GENESIS_SINGLE_USER", "true")
        .env("GENESIS_LOG_JSON", "true")
        .stdout(out)
        .stderr(err)
        .spawn();

    match child {
        Ok(process) => {
            *engine.0.lock().unwrap() = Some(process);
            if wait_for_health() {
                EngineStatus {
                    running: true,
                    api_base: API_BASE.to_string(),
                    token: read_session().map(|s| s.access_token),
                    refresh_token: read_session().map(|s| s.refresh_token),
                    message: "Engine started".into(),
                }
            } else {
                // Name the log rather than only the symptom: the engine's own
                // output is the diagnosis, and the user cannot guess where it
                // went.
                EngineStatus {
                    running: false,
                    api_base: API_BASE.to_string(),
                    token: None,
                    refresh_token: None,
                    message: format!(
                        "The engine started but never answered /health. See {}",
                        log_path.display()
                    ),
                }
            }
        }
        // A failed spawn has a small number of causes and the OS distinguishes
        // them, so the message should too. "Permission denied" after a fresh
        // checkout means the executable bit was lost — an archive or a
        // snapshot will do that — and telling the user to run chmod is more
        // useful than reporting the raw errno.
        Err(err) => {
            let hint = match err.kind() {
                std::io::ErrorKind::NotFound => format!(
                    "{binary} was not found. Build it with `make build`, \
                     or put genesis-server on PATH."
                ),
                std::io::ErrorKind::PermissionDenied => format!(
                    "{binary} is not executable. Run `chmod +x {binary}` \
                     — archives and workspace snapshots drop the executable bit."
                ),
                _ => format!("Could not launch {binary}: {err}"),
            };
            EngineStatus {
                running: false,
                api_base: API_BASE.to_string(),
                token: None,
                refresh_token: None,
                message: hint,
            }
        }
    }
}

fn wait_for_health_once() -> bool {
    ureq::get(&format!("{API_BASE}/health"))
        .timeout(Duration::from_millis(400))
        .call()
        .map(|r| r.status() == 200)
        .unwrap_or(false)
}

#[tauri::command]
fn engine_status(engine: State<Engine>) -> EngineStatus {
    if wait_for_health_once() {
        return EngineStatus {
            running: true,
            api_base: API_BASE.to_string(),
            token: read_session().map(|s| s.access_token),
            refresh_token: read_session().map(|s| s.refresh_token),
            message: "Engine is healthy".into(),
        };
    }
    start_engine(&engine)
}

#[tauri::command]
fn session_token() -> Option<String> {
    read_session().map(|s| s.access_token)
}

/// Re-reads the session file from disk.
///
/// The server rewrites it whenever it issues a local session, so re-reading is
/// how the UI recovers after the engine restarts underneath it.
#[tauri::command]
fn session_tokens() -> Option<SessionFile> {
    read_session()
}

/// Opens the engine log in the platform's default viewer.
#[tauri::command]
fn open_engine_log() -> Result<(), String> {
    let path = data_dir().join("engine.log");
    if !path.exists() {
        return Err(format!("No engine log yet at {}", path.display()));
    }
    reveal(&path)
}

/// Opens the directory holding projects, the database and the session file.
#[tauri::command]
fn open_data_dir() -> Result<(), String> {
    let dir = data_dir();
    std::fs::create_dir_all(&dir).map_err(|e| e.to_string())?;
    reveal(&dir)
}

#[tauri::command]
fn open_workspace(path: String) -> Result<(), String> {
    // Delegating to the platform file manager avoids shipping our own, and the
    // path always originates from the server, never from user input.
    #[cfg(target_os = "windows")]
    let result = Command::new("explorer").arg(&path).spawn();
    #[cfg(target_os = "macos")]
    let result = Command::new("open").arg(&path).spawn();
    #[cfg(all(unix, not(target_os = "macos")))]
    let result = Command::new("xdg-open").arg(&path).spawn();

    result.map(|_| ()).map_err(|e| e.to_string())
}

/// Opens a path with the platform's default handler.
fn reveal(path: &std::path::Path) -> Result<(), String> {
    #[cfg(target_os = "windows")]
    let result = Command::new("explorer").arg(path).spawn();
    #[cfg(target_os = "macos")]
    let result = Command::new("open").arg(path).spawn();
    #[cfg(all(unix, not(target_os = "macos")))]
    let result = Command::new("xdg-open").arg(path).spawn();

    result.map(|_| ()).map_err(|e| e.to_string())
}

/// Stops the engine and starts it again.
#[tauri::command]
fn restart_engine(engine: State<Engine>) -> EngineStatus {
    let existing = engine.0.lock().unwrap().take();
    if let Some(mut child) = existing {
        let _ = child.kill();
        let _ = child.wait();
    }
    // Give the port time to be released; binding it again immediately fails
    // on Windows, where a closed socket lingers in TIME_WAIT.
    std::thread::sleep(Duration::from_millis(700));
    start_engine(&engine)
}

fn main() {
    tauri::Builder::default()
        .manage(Engine(Mutex::new(None)))
        .invoke_handler(tauri::generate_handler![
            engine_status,
            restart_engine,
            session_token,
            session_tokens,
            open_workspace,
            open_engine_log,
            open_data_dir
        ])
        .setup(|app| {
            let engine: State<Engine> = app.state();
            let status = start_engine(&engine);

            // Inject the loopback token before the frontend boots so the first
            // API call is already authenticated and no login flash occurs.
            if let Some(token) = status.token {
                if let Some(window) = app.get_webview_window("main") {
                    let script = format!(
                        "window.__GENESIS_TOKEN__ = {}; window.__GENESIS_REFRESH__ = {};",
                        serde_json::to_string(&token).unwrap_or_else(|_| "null".into()),
                        serde_json::to_string(&status.refresh_token.unwrap_or_default())
                            .unwrap_or_else(|_| "null".into())
                    );
                    let _ = window.eval(&script);
                }
            }
            Ok(())
        })
        .on_window_event(|window, event| {
            if let tauri::WindowEvent::Destroyed = event {
                // Terminate the child explicitly. An orphaned engine holding
                // port 8787 would break the next launch.
                //
                // The child is moved out of the mutex on its own statement,
                // and the guard is dropped at that semicolon. Writing this as
                // `if let Some(mut child) = engine.0.lock().unwrap().take()`
                // does not compile: the temporaries in the scrutinee — both
                // the `State` borrowed from `window` and the `MutexGuard` —
                // live until the end of the `if let`, so the guard outlives
                // the `State` it was taken from. Rust 2024 tightened this;
                // under older toolchains it compiled while holding the lock
                // across `child.wait()`, which is a latent deadlock rather
                // than a safe program.
                let engine = window.state::<Engine>();
                let child = engine.0.lock().unwrap().take();

                if let Some(mut child) = child {
                    let _ = child.kill();
                    let _ = child.wait();
                }
            }
        })
        .run(tauri::generate_context!())
        .expect("failed to start the Genesis desktop shell");
}
