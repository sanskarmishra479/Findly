# Phase 3 preparation (checked 2026-10-12)

Phase 3 (the desktop app) runs **before Phase 2**, pending the user's OK on the phase order.

## Toolchain on the dev PC

| Tool | Version | Status |
|---|---|---|
| Go | 1.27.2 | ✅ |
| Wails CLI | v2.16.0 | ✅ matches the library version |
| Node.js / npm | 24.19.0 / 11.17.0 | ✅ Vite 8 needs Node ≥ 22.12 |
| WebView2 runtime | 154.0 | ✅ (preinstalled on Windows 11; the installer must bootstrap it on Windows 10) |
| NSIS (`makensis`) | — | ❌ only needed for the installer (Phase 7) |
| RAM | 15.7 GB | ✅ |

## Wails: v2, not v3

- **v2.16.0** (14 Sep 2026) is the stable line. **v3 is still a beta** (v3.0.0-beta.28, 5 Oct 2026, no announced release date), so under the stable-only rule we use v2.
- From the v2.16.0 source, these options exist: `Frameless`, `StartHidden`, `HideWindowOnClose`, `AlwaysOnTop`, `DisableResize`, `SingleInstanceLock` (with `OnSecondInstanceLaunch`), `windows.WebviewUserDataPath`.
- **Not in v2:** a tray icon and a global hotkey. We build both on Windows APIs through `golang.org/x/sys/windows`, with no new library:
  - Hotkey: `RegisterHotKey` on a dedicated, locked OS thread with its own message loop. Registration fails if another app owns the key, which gives us conflict detection for free.
  - Tray: `Shell_NotifyIconW` with a hidden message-only window (`HWND_MESSAGE`), and `TrackPopupMenu` for the menu.
- **Must fix:** WebView2 stores its browser data in `%APPDATA%\<exe>` by default. That breaks I-3, so set `WebviewUserDataPath` to `%LOCALAPPDATA%\Findly\webview`.
- **Security check:** a minimal Wails v2 app links only 8 modules (wails/v2, go-webview2, x/sys, pkg/errors, uniseg and 3 small leaanthony helpers). `govulncheck` reports no vulnerabilities. Size is about 6 MB.

## Frontend (React + TypeScript + Vite, plan section 4)

Use exact version pins, a committed `package-lock.json`, `npm ci` and `npm audit`. We write our own `package.json` instead of the Wails template's old versions.

| Package | Version | Released |
|---|---|---|
| react / react-dom | 19.3.0 | 2026-09-09 |
| vite | 8.3.x | 2026-10 |
| @vitejs/plugin-react | 6.1.x | 2026-10 |
| typescript | 7.0.2 | 2026-07-08 |

- Vite's 2026 advisories (GHSA-fx2h-pf6j-xcff, GHSA-p9ff-h696-f583, GHSA-v2wj-q39q-566r) affect only its **dev server**, which is never shipped. Use the latest patch anyway, and keep the dev server on localhost.
- **P-2 (no remote resources):** use the system font (Segoe UI), bundled icons only, and a strict Content-Security-Policy (`default-src 'self'`).
- Turn devtools off in release builds. Go functions exposed to the UI are limited to the app API (no file writes; I-6 actions only).

## Other checks

- **Model download (task 2):** Hugging Face answers Range requests (HTTP 206 through `cdn.hf.co`) without a login, so parallel resumable downloads work. The SHA-256 values are already pinned in `scripts/fetch-model.ps1`.
- **Opener (task 6):** Edge is at `C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`, for `file:///…pdf#page=N`. Use `ShellExecute` for the default app and `explorer.exe /select,` for "Show in Explorer"; a `CF_HDROP` clipboard copy works through x/sys/windows.
- **Backend auto-selection (task 7):** compare against `testdata/reference_vectors.json`. Use Vulkan only if its vectors match the CPU ones (cosine ≥ 0.999, as in `cli compare`), otherwise use the CPU.
- **Hotkey:** Ctrl+Shift+Space is the default (plan 10.2). Some Chinese/Japanese input methods use Ctrl+Space, and our combination includes Shift, so it doesn't collide; it stays configurable.
- **Opening the panel in < 150 ms:** create the window hidden at startup, and on the hotkey only show and focus it. Verify that `SetForegroundWindow` is allowed after a hotkey (it normally is for the process that receives `WM_HOTKEY`).
- **Autostart:** an HKCU `Run` value, opt-in in settings, removed on uninstall (I-8).

## Speed items folded into Phase 3 (from the indexing investigation)

1. **Keyword first:** store a file's passages and FTS rows as soon as it is extracted; add vectors in a background pass, so files become searchable by words within seconds.
2. **Start the model in parallel** with scanning and extraction (saves ~7 s per indexing run).
3. **Search model lifetime:** start it when the app starts, and on the hotkey if it was unloaded. Keyword results appear instantly, and meaning results merge in when the model is ready. Unload policy is pending the user's answer: keep it loaded on PCs with ≥ 8 GB RAM on AC power, otherwise unload after 10 min idle.

## Task order for coding

| # | Task | Plan ref |
|---|---|---|
| T1 | Wails v2 skeleton: `app/` + `frontend/`, pinned deps, WebView data path in the app folder, single instance, CSP, start hidden | 3.1, P-2, I-3 |
| T2 | Tray icon and global hotkey (x/sys/windows, own message loop) | 10.2, 10.4 |
| T3 | Engine service: background indexer, model lifetime, keyword-first indexing, progress events | 5, 9.10 |
| T4 | Search panel: frameless, keyword-first + meaning merge, cancellation, keyboard navigation | 10.2 |
| T5 | `opener`: open, open PDF at page, show in Explorer, copy file | 9.15, I-6 |
| T6 | `model` download (parallel, resume, SHA-256, import) + onboarding with folder picker | 9.13, 10.1 |
| T7 | Settings (folders, performance, privacy, about) + basic big window | 10.3, 10.5 |
| T8 | Backend auto-selection (Vulkan vs CPU) | 4, 14.2 |
| — | Acceptance: fresh user account, panel < 300 ms, no network except the model download | 13 |

## Open questions for the user

1. Phase order: Phase 3 before Phase 2?
2. Search model: keep it loaded (~400 MB) on PCs with ≥ 8 GB RAM on AC power?
3. App icon: is there a logo, or should a simple placeholder be used for now?
