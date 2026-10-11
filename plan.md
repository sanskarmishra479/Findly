# Project Plan: Local Semantic File Search for Windows

> Working name: **Findly** (may change before release; must NOT contain "Gemma" or "Google").
> - **DisplayName** = `Findly`: user-facing brand (window titles, UI text, installer name). Safe to change any time; define it in one constant.
> - **AppID** = `findly`: internal ID used for the app data folder, license `product` field, device ID salt and installer ID. **Frozen after the first release** even if the brand changes, because changing it breaks existing installs and licenses.
> Go module path: `github.com/sanskarmishra479/findly`.
> Inspired by DigUp (macOS, MIT license): https://github.com/ARahim3/DigUp — read its README and `scripts/build-llama.sh` before starting.

---

## 0. How to use this document (instructions for Claude Code)

1. Work **phase by phase** (Section 13). Do not start a phase until the previous phase's acceptance criteria pass.
2. Section 3 (**Invariants**) is non-negotiable. If any task seems to require breaking one, stop and ask.
3. Items marked **VERIFY** depend on fast-moving libraries or APIs. Check current official docs before implementing, and record what you found in `docs/decisions.md`.
4. Prefer the stack in Section 4. Ask before swapping a library.
5. Every phase ends with tests. Never mark a phase done with failing tests.
6. Keep a running `docs/decisions.md` (what was decided and why) and `docs/progress.md` (checklist of phases and tasks).
7. No cgo anywhere in the Go code. Native code lives in separate helper executables.

---

## 1. Product summary

A Windows desktop app that lets users **search their files by what's inside them**, in their own words, fully on their PC:

- "zebra in a video" → opens the clip at the moment the zebra appears
- "where they talk about sleep" → jumps to that minute of a recording
- "the clause about pets in the lease" → opens the PDF at the right page
- "a dog on the beach" → finds the photo; "payment declined error" → finds the screenshot
- Exact strings (invoice numbers, error codes, names) are matched too, via a keyword index
- Works across 100+ languages

Built on **Google DeepMind's EmbeddingGemma 2** (open model, Apache 2.0), run locally through **llama.cpp**. Paid app: **$5 lifetime license**, with a free tier.

---

## 2. Goals and non-goals

### Goals
- Fast search-as-you-type: keyword results < 50 ms, semantic results < 300 ms after typing pauses, even on a low-end PC.
- Runs on low-end hardware (8 GB RAM, CPU only), slower first indexing is acceptable.
- Small installer (target < 40 MB). Model downloaded on first run, in stages.
- Search usable within minutes of install; rest of indexing continues in the background.
- Fully offline after setup. Zero telemetry.
- Never modifies, moves, deletes or locks user files.

### Non-goals (v1)
- No cloud sync, no accounts, no mobile app.
- No generative AI / chat over files (embeddings + search only).
- No macOS/Linux build.
- No code search in v1 (optional later phase).

---

## 3. Invariants (must always hold)

### 3.1 File safety: never lose or change user files
- **I-1** All reads of user files go through `internal/safefs`. This package exposes **read-only** functions only. It has no write, rename, move, delete or truncate function.
- **I-2** User files are opened with `FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE` (via `golang.org/x/sys/windows.CreateFile`) and `GENERIC_READ` only, so the app never blocks other programs from saving, renaming or deleting.
- **I-3** All writes go through `internal/appfs`, which refuses any path outside the app data directory (`%LOCALAPPDATA%\Findly\`). Resolve and clean paths before checking; reject `..`, symlinks and junctions that escape the directory.
- **I-4** Never trigger cloud file downloads: skip files with `FILE_ATTRIBUTE_RECALL_ON_DATA_ACCESS` or `FILE_ATTRIBUTE_RECALL_ON_OPEN` or `FILE_ATTRIBUTE_OFFLINE`.
- **I-5** Do not follow symlinks or junctions (reparse points) during scanning.
- **I-6** UI actions on results are limited to: open with default app, open at page/timestamp/line, show in Explorer, copy file to clipboard. No delete/move/rename actions.
- **I-7** A corrupt index must never affect user files. Index corruption → offer "rebuild index".
- **I-8** Uninstall removes only `%LOCALAPPDATA%\Findly\` and app settings.
- **I-9** An automated **file-integrity test** runs in CI and before every release (Section 14.3).

### 3.2 Privacy: no tracking
- **P-1** No telemetry, analytics, crash auto-upload, or accounts. Ever.
- **P-2** The UI loads **no remote resources**: fonts, icons, scripts and styles are bundled.
- **P-3** Logs are local only, in the app data dir, rotated (max ~5 MB × 3 files). A "Copy log" button lets users send it manually if they want.
- **P-4** `llama-server` binds to `127.0.0.1` only, on a random free port, with a random per-session API key.
- **P-5** When a file is deleted, or a folder is removed from the app, its text, vectors and thumbnails are deleted from the index immediately.
- **P-6** Never read secret-looking files (Section 9.3 skip rules), even inside chosen folders.

### 3.3 Offline
- **O-1** After setup, the app makes **zero automatic network connections**.
- **O-2** The only network uses are: model download (first run, or when a new modality is needed), license activation (once, user-initiated), and a manual "Check for updates" button.
- **O-3** Model download can be replaced by "Import model files" from disk.
- **O-4** The app must fully work (index + search) with networking disabled. Tested before each release.

### 3.4 Licensing never blocks the user
- **L-1** License verification is offline: a signed activation file verified with a public key compiled into the app.
- **L-2** The app **never** checks the license online at launch.
- **L-3** If the license is missing or invalid, the app falls back to the free tier. It **never** blocks searching already-indexed files.

---

## 4. Tech stack

| Area | Choice | Notes |
|---|---|---|
| Language | Go (latest stable) | No cgo |
| Desktop shell | **Wails** | VERIFY current major version and its tray/global-hotkey support |
| Frontend | React + TypeScript + Vite | Bundled; no CDN |
| Embedding model | **EmbeddingGemma 2**, GGUF, 8-bit (Q8_0) | ggml-org / unsloth GGUF builds. VERIFY file names, sizes, SHA-256 |
| Inference | **llama.cpp `llama-server.exe`** as sidecar | Pin a release that includes EmbeddingGemma 2 support (merged upstream on 2026-10-06, PR #30054). Ship Vulkan + CPU builds |
| Database | **SQLite** via `github.com/ncruces/go-sqlite3` (pure Go, wasm) | FTS5 for keywords. VERIFY FTS5 availability in ncruces build |
| Vector storage | Custom: binary + int8 blobs in SQLite, scanned in Go | Optionally sqlite-vec via ncruces bindings later |
| PDF | `github.com/klippa-app/go-pdfium` in **WebAssembly mode** | No cgo. VERIFY wasm mode API |
| DOCX / ODT | Parse zip + XML in Go | No external deps |
| RTF / HTML | Lightweight Go stripping | |
| OCR | **`ocr-helper.exe`** (small C# Native AOT or Rust program) using `Windows.Media.Ocr` | JSON over stdin/stdout |
| Audio/video | **`ffmpeg.exe`**, downloaded on demand | Only when user picks folders with media |
| File watching | `github.com/fsnotify/fsnotify` + NTFS USN journal via `golang.org/x/sys/windows` | |
| Global hotkey | `golang.design/x/hotkey` or `RegisterHotKey` directly | |
| Licensing | `crypto/ed25519` (Go std) + Cloudflare Worker + Dodo Payments license keys | |
| Installer | `wails build -nsis` | Code signing later |

---

## 5. Architecture

```
┌──────────────────────────── Findly.exe (Go, Wails) ────────────────────────────┐
│  React UI  ◄──Wails bindings──►  app/ (UI-facing API)                               │
│                                     │                                               │
│   internal/search ◄─── internal/store (SQLite: files, items, vectors, FTS5) ───►    │
│        ▲                          ▲                                                 │
│        │                          │                                                 │
│   internal/embed  ◄────── internal/index (pipeline, queue, priorities) ◄── watch    │
│        │                          │                                                 │
│        │                    internal/extract ── internal/safefs (read-only)         │
│        │                          │                                                 │
└────────┼──────────────────────────┼─────────────────────────────────────────────────┘
         │ HTTP 127.0.0.1           │ stdin/stdout JSON         │ process
         ▼                          ▼                           ▼
   llama-server.exe            ocr-helper.exe               ffmpeg.exe
   (EmbeddingGemma 2)          (Windows.Media.Ocr)          (frames + audio)
```

### Process model
- **Main app** is always running (tray).
- **llama-server (indexing mode)**: started when there is indexing work; loaded with the modalities needed; stopped after 60 s idle.
- **llama-server (search mode)**: text-only, started when the app launches or the hotkey is first pressed; kept warm while the panel is open; unloaded after ~2 min idle. VERIFY whether one server instance can serve both roles efficiently; if so, use one.
- **ocr-helper**: started on demand, kept alive while indexing images, stopped when idle.
- **ffmpeg**: one process per media file job.
- All sidecars: started with low process priority during indexing (`BELOW_NORMAL_PRIORITY_CLASS`), restarted automatically on crash (max 3 retries, then mark the item failed and continue).

---

## 6. Repository layout

```
findly/
├─ cmd/
│  └─ cli/                 # dev CLI: index, search, eval, bench
├─ app/                    # Wails app entry, bindings exposed to UI
├─ frontend/               # React + TS (Vite)
├─ internal/
│  ├─ appfs/               # writes, ONLY inside app data dir (I-3)
│  ├─ safefs/              # read-only access to user files (I-1, I-2, I-4, I-5)
│  ├─ config/              # settings load/save (JSON in app data dir)
│  ├─ scan/                # folder walking, skip rules, file classification
│  ├─ extract/             # text, pdf, docx, odt, rtf, html, image prep, media
│  ├─ chunk/               # text splitting into passages
│  ├─ ocr/                 # client for ocr-helper.exe
│  ├─ embed/               # llama-server client, batching, prefixes
│  ├─ vecfmt/              # truncation, normalization, int8 + binary encoding
│  ├─ store/               # SQLite schema, migrations, queries
│  ├─ index/               # pipeline, queue, priorities, progress, estimates
│  ├─ search/              # keyword + vector + fusion
│  ├─ watch/               # fsnotify, debounce, USN catch-up
│  ├─ thumbs/              # thumbnail generation + cache
│  ├─ model/               # manifest, staged parallel download, verify, import
│  ├─ sidecar/             # process lifecycle for helpers
│  ├─ opener/              # open file / page / timestamp / Explorer
│  ├─ power/               # battery saver / AC status
│  └─ license/             # activation file verification, free tier
├─ helpers/
│  └─ ocr-helper/          # C# (Native AOT) or Rust project
├─ server/
│  └─ activation-worker/   # Cloudflare Worker (TypeScript)
├─ evals/                  # queries.jsonl + testbed builder
├─ testdata/               # small fixture files for tests
├─ scripts/                # build llama.cpp, fetch binaries, release
└─ docs/                   # decisions.md, progress.md, privacy.md
```

---

## 7. Model details (EmbeddingGemma 2)

Facts from the official model card (https://huggingface.co/google/embeddinggemma-2):

- **740M parameters total** = text model 270M + vision encoder 170M + audio encoder 300M. Encoders are independently loadable.
- Approx 8-bit sizes: text ~300 MB, vision ~180 MB, audio ~320 MB (VERIFY exact file sizes of the GGUF files you use).
- Output: **768-d**. Supports **Matryoshka truncation** to 512/256/128. **Use 256-d** (near-lossless). Do NOT use 128-d (hurts image/audio/video quality).
- After truncation, **L2-normalize** again. Queries and documents must use the same dimension.
- **Precision:** run in bfloat16 or float32. **Never float16** (silently produces NaN or degraded vectors). Verify GPU backends with reference vectors (Section 14.2).
- **Context:** 8,192 tokens shared across modalities.
- **Image tokens:** default 280 per image; configurable 70–1120. **First pass: 70.** Optional idle "refine" pass at 280.
- **Video:** default 1 fps at 140 tokens/frame. We sample far fewer frames (Section 9.6).
- **Audio:** mono, 16 kHz, 25 tokens/second.
- **Text prefixes (important for quality):**
  - Search query: `task: search result | query: {query}`
  - Document passage: `title: {title or "none"} | text: {content}` (use the file name as title)
  - Code (later phase): query `task: code retrieval | query: {query}`, doc `title: {filename} | text: {code}`
  - Images, audio, video: **no prefix**.
- **VERIFY:** how `llama-server` accepts image and audio inputs for embeddings in the pinned llama.cpp version (likely via a multimodal projector file / `mtmd`), the exact CLI flags (`--embeddings`, pooling mode, projector args), and the request format. Check DigUp's `scripts/build-llama.sh` for its one-line patch and whether it's still needed.

### Model manifest
Ship a manifest compiled into the app:

```json
{
  "model_version": "embeddinggemma-2-q8_0-<llamacpp-tag>",
  "parts": [
    { "name": "text",   "file": "...gguf", "size": 0, "sha256": "...", "urls": ["https://huggingface.co/...", "https://<your-mirror>/..."] },
    { "name": "vision", "file": "...gguf", "size": 0, "sha256": "...", "urls": [...] },
    { "name": "audio",  "file": "...gguf", "size": 0, "sha256": "...", "urls": [...] }
  ]
}
```

The index stores `model_version`. If it changes in a future release, prompt the user before re-indexing.

---

## 8. Data model (SQLite)

File: `%LOCALAPPDATA%\Findly\index\index.db`, WAL mode, `synchronous=NORMAL`, foreign keys on.

```sql
CREATE TABLE meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
); -- schema_version, model_version, vector_dim, created_at

CREATE TABLE roots (
  id        INTEGER PRIMARY KEY,
  path      TEXT NOT NULL UNIQUE,      -- folder chosen by the user
  enabled   INTEGER NOT NULL DEFAULT 1,
  added_at  INTEGER NOT NULL
);

CREATE TABLE excludes (
  id       INTEGER PRIMARY KEY,
  root_id  INTEGER REFERENCES roots(id) ON DELETE CASCADE,
  path     TEXT NOT NULL               -- subfolder unticked by the user
);

CREATE TABLE files (
  id           INTEGER PRIMARY KEY,
  root_id      INTEGER NOT NULL REFERENCES roots(id) ON DELETE CASCADE,
  path         TEXT NOT NULL UNIQUE,
  name         TEXT NOT NULL,
  ext          TEXT NOT NULL,
  kind         TEXT NOT NULL,           -- text | doc | pdf | image | audio | video
  size         INTEGER NOT NULL,
  mtime        INTEGER NOT NULL,        -- unix ns
  content_hash TEXT,                    -- xxhash/sha256 of content, for dedupe + moves
  status       TEXT NOT NULL,           -- pending | partial | done | failed | skipped
  quality      TEXT,                    -- quick | full (image budget used)
  error        TEXT,
  indexed_at   INTEGER
);
CREATE INDEX files_status ON files(status);
CREATE INDEX files_hash ON files(content_hash);

CREATE TABLE items (
  id        INTEGER PRIMARY KEY,
  file_id   INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  kind      TEXT NOT NULL,              -- passage | page | image | frame | audio
  locator   TEXT,                       -- JSON: {"page":3} | {"t":125.0,"dur":30} | {"chunk":7}
  text      TEXT                        -- passage text or OCR text (may be NULL)
);
CREATE INDEX items_file ON items(file_id);

CREATE TABLE vectors (
  item_id  INTEGER PRIMARY KEY REFERENCES items(id) ON DELETE CASCADE,
  bin      BLOB NOT NULL,               -- 256 bits = 32 bytes (sign bits)
  i8       BLOB NOT NULL,               -- 256 bytes int8
  scale    REAL NOT NULL                -- per-vector scale for int8 dequantization
);

CREATE VIRTUAL TABLE items_fts USING fts5(
  name, text,
  content='',                           -- contentless; rowid = items.id
  tokenize='unicode61 remove_diacritics 2'
);

CREATE TABLE thumbs (
  file_id  INTEGER PRIMARY KEY REFERENCES files(id) ON DELETE CASCADE,
  path     TEXT NOT NULL                -- relative path inside app data thumbs/
);
```

- Deleting a `files` row must cascade to items, vectors, thumbs, and the FTS rows (FTS5 contentless tables need explicit `DELETE` via the special insert command; implement in `store`).
- A file name–only FTS row is inserted for every file (even unsupported kinds) so file-name search always works.

---

## 9. Component specifications

### 9.1 `safefs` (read-only)
- `Open(path) (io.ReadSeekCloser, error)` using `CreateFile` with `GENERIC_READ` and full share flags (I-2), `FILE_FLAG_SEQUENTIAL_SCAN`.
- `Stat(path)` returns size, mtime, attributes. `IsCloudOnly(attrs) bool` (I-4). `IsReparsePoint(attrs) bool` (I-5).
- `WalkDir(root, fn)` that never descends into reparse points.
- Unit tests: open a file, then in parallel rename + delete + write it from the test; all must succeed while our handle is open.

### 9.2 `appfs` (writes only inside app dir)
- `Root()` → `%LOCALAPPDATA%\Findly\`. Subdirs: `models\`, `index\`, `thumbs\`, `logs\`, `bin\` (downloaded helpers like ffmpeg), `license\`.
- Every write API takes a path relative to `Root()` and rejects anything that resolves outside it.
- Atomic writes: write to `.tmp`, `fsync`, rename.

### 9.3 `scan` (what to read)
- Inputs: roots + excludes from DB.
- Classify by extension:
  - text: `.txt .md .markdown .csv(text only, see below) .log .json(skip if huge)`
  - doc: `.docx .odt .rtf .html .htm`
  - pdf: `.pdf`
  - image: `.jpg .jpeg .png .webp .bmp .gif(first frame) .tif .tiff .heic(if WIC codec present)`
  - audio: `.mp3 .m4a .wav .flac .ogg .opus .aac .wma`
  - video: `.mp4 .mkv .mov .avi .webm .wmv .m4v`
- **Always skip:** hidden/system files and folders; `AppData`, `$Recycle.Bin`, `System Volume Information`, `node_modules`, `.git`, `.venv`, `venv`, `__pycache__`, `.cache`, `Thumbs.db`, `desktop.ini`; folders containing `.git`/`package.json`/`go.mod`/`Cargo.toml`/`pom.xml` (code projects: index **only images** inside them, like DigUp); `.ssh`, `.gnupg`, `.aws`, `.azure`, `.kube`; files named like keys or secrets: `*.pem *.key *.pfx *.p12 *.kdbx *.ovpn id_rsa* id_ed25519* *.env .env*`, `*password*`, `*secret*`; files > 2 GB except video; images smaller than 128×128 (icons); cloud-only files (I-4); reparse points (I-5).
- Text mostly digits (> 60% digits/punctuation, e.g. data exports) → FTS only, no embedding.
- Outputs counts per root per kind (for estimates) quickly, without reading file contents.

### 9.4 `extract`
- **Text files:** detect encoding (UTF-8, UTF-16 with BOM; fallback Windows-1252), normalize newlines.
- **DOCX/ODT:** unzip, parse `word/document.xml` / `content.xml`, keep paragraph breaks.
- **RTF/HTML:** strip markup to text.
- **PDF (go-pdfium wasm):** per page: extract text. If a page has < 20 chars of text, render it to an image at ~150 DPI and send to OCR (scanned page). Long PDFs: first 30 pages in the first pass, rest deferred to a low-priority job (cap 5,000 pages).
- **Images:** decode with Go image libs (+ `golang.org/x/image/webp`, `tiff`, `bmp`); downscale so the long side ≤ 1024 px before embedding; fix EXIF orientation. HEIC: VERIFY a no-cgo path (WIC via helper, or skip with a clear message).
- **Audio/video:** via ffmpeg (Section 9.6).

### 9.5 `chunk`
- Split text into passages of ~350 tokens (estimate ~4 chars/token) with ~50-token overlap, preferring paragraph and sentence boundaries.
- Each passage stored as an item with `locator {"chunk":n}` (and `{"page":p}` for PDFs).
- Embed with document prefix: `title: {filename} | text: {passage}`.

### 9.6 Media (audio + video) via ffmpeg
- ffmpeg is downloaded on demand (with SHA-256 check) only if any root contains audio/video.
- **Audio:** `ffmpeg -i in -ac 1 -ar 16000 -f s16le -` → split into **30 s windows with 10 s overlap**; each window = one item `{"t":start,"dur":30}`.
- **Video frames (first pass):** scene-change sampling: `ffmpeg -i in -vf "select='gt(scene,0.3)',scale=512:-1" -vsync vfr ...`, plus at least one frame every 10 s, max 1 frame per 2 s. Each frame = one item `{"t":timestamp}`.
- **Video audio:** extract soundtrack and process like audio.
- Skip media longer than 4 hours in the first pass (defer to low priority).

### 9.7 `ocr` (ocr-helper.exe protocol)
- Long-lived process; JSON lines over stdin/stdout.
- Request: `{"id":1,"path":"C:\\...\\img.png","lang":"auto"}` or `{"id":1,"bytes_b64":"..."}` for rendered PDF pages.
- Response: `{"id":1,"ok":true,"text":"...","lines":[{"text":"...","box":[x,y,w,h]}]}` or `{"id":1,"ok":false,"error":"..."}`.
- Uses `Windows.Media.Ocr.OcrEngine.TryCreateFromUserProfileLanguages()`; report available languages via a `{"cmd":"langs"}` request so Settings can show them.
- The helper must open files read-only (it never writes anything).
- OCR text goes into the item's `text` (FTS). If OCR text > 200 chars, also create a separate `passage` item embedded as text.

### 9.8 `embed` (llama-server client)
- Starts llama-server with: model file(s) for needed modalities, embeddings mode, pooling per model requirements, `--host 127.0.0.1 --port <random>`, `--api-key <random>`, thread count = `max(1, cores/2)` during indexing. VERIFY all flags for the pinned version.
- Backend selection on first run: try Vulkan; run reference-vector check (Section 14.2); if mismatch or failure, fall back to CPU. Store the result in settings.
- Batch requests (e.g. 16 texts or 4 images per request; tune in Phase 0).
- Output processing: take first 256 dims → L2 normalize → `vecfmt` encode.
- Query embedding API: `EmbedQuery(ctx, q)` with LRU cache (256 entries), cancellable via context.

### 9.9 `vecfmt`
- `Truncate256(v []float32) []float32` + normalize.
- `ToInt8(v) (q []int8, scale float32)`: scale = max(|v|)/127.
- `ToBinary(v) [32]byte`: bit = 1 if component > 0.
- `Hamming(a, b [32]byte) int` using `math/bits.OnesCount64`.
- `DotInt8(q []int8, qs float32, d []int8, ds float32) float32`.
- Benchmarks required: binary scan of 100k vectors < 5 ms on a mid laptop.

### 9.10 `index` (pipeline)
- Stages connected by channels: **scan → extract → (ocr) → embed → store**. Worker pools per stage; embed stage is a single batching worker feeding llama-server.
- **Priority queue order:** (1) new/changed files from the watcher, (2) files modified in the last 30 days, (3) Desktop/Documents/Downloads, (4) text/docs/pdf, (5) images, (6) audio, (7) video, (8) deferred PDF pages, (9) refine pass (images at 280 tokens) only when idle + on AC power.
- **Dedupe:** if `content_hash` already exists for another path, copy its items/vectors instead of re-embedding.
- **Change detection:** skip if size + mtime unchanged.
- **Power:** pause when Battery Saver is on; optional setting "only index when plugged in".
- **Pause/resume** from tray. **Progress events** to UI: per root, per kind, ETA.
- **Estimates:** time first N items of each kind (N=5) on this PC, multiply by counts, show per root. Refine as indexing proceeds.
- Failures: mark file `failed` with error; never stop the pipeline.
- **Free tier:** when unlicensed, content indexing (text, OCR, vectors) stops after **300 files**; the first 300 are picked by the normal priority order. File-name search still covers **all** files in the chosen folders. Removing or deleting indexed files frees slots. Everything already indexed stays searchable (L-3). After purchase, indexing resumes from where it stopped.

### 9.11 `search`
- `Search(ctx, query, filters)` called on every keystroke from the UI.
- **Keyword path (every keystroke, immediately):** FTS5 MATCH over `name` and `text` with prefix matching on the last token, bm25 ranking, limit 50. Emit results to UI immediately.
- **Semantic path (after 120 ms debounce, cancelled on new keystroke):**
  1. Embed query with search prefix (cached).
  2. Binary Hamming scan over all vectors in RAM → top 200.
  3. Re-rank those 200 with int8 dot product.
  4. Group by file (best item per file, keep the locator of the best item).
- **Fusion:** Reciprocal Rank Fusion (k=60) of keyword and semantic lists, plus an **exact-match boost** when the query (or a quoted part of it) appears verbatim in name or text.
- Filters: kind (images/docs/pdf/audio/video), folder, date range.
- **In-memory vector cache:** load all `bin` blobs into a contiguous `[]byte` at startup (100k items ≈ 3.2 MB); `i8` loaded lazily or kept in RAM if < 100 MB.
- Optional `"quoted"` syntax forces exact match.

### 9.12 `watch`
- fsnotify on each root (recursive). Debounce per file: wait until size is stable for 1 s.
- Events: create/modify → enqueue (priority 1); delete → remove from index immediately (P-5); rename → update path (match by hash if delete+create).
- **Startup catch-up:** read the NTFS USN journal since the last stored USN for each volume; fallback to an mtime scan when the journal is unavailable or wrapped. VERIFY USN API usage via `x/sys/windows` (`FSCTL_QUERY_USN_JOURNAL`, `FSCTL_READ_USN_JOURNAL`); requires only read access to the volume for the user's own files (VERIFY privileges; fallback to scan if access denied).

### 9.13 `model` (download)
- Staged: **text first**, then **vision**, then **audio only if needed**.
- Parallel ranged download: 4–6 connections per file, each to a `.partN` file; resume using existing part sizes; merge; **SHA-256 verify**; atomic rename into `models\`.
- Retry with backoff; try mirror URLs in order.
- Pause/resume buttons; warn on metered connections (`NetworkInformation` cost via helper, or skip warning in v1).
- **Import:** user picks local GGUF files; verify hash; copy into `models\`.
- While downloading: scan, extract text, OCR and thumbnails continue (they don't need the model).

### 9.14 `thumbs`
- 256 px long side JPEG (quality 75) for images, video (frame at best-match timestamp later; at index time use the first sampled frame), and PDF first page.
- Stored under `thumbs\<first 2 hex of id>\<id>.jpg`.

### 9.15 `opener`
- Default: `ShellExecute "open"`.
- PDF at page: open in Edge with `file:///...pdf#page=N`, or SumatraPDF `-page N` if installed. Configurable.
- Audio/video at timestamp: prefer built-in preview player in the app (HTML5 `<video>` via a local file server bound to 127.0.0.1 with random token), or VLC `--start-time=<s>` / mpv `--start=<s>` if installed.
- Show in Explorer: `explorer.exe /select,"<path>"`.
- Copy: put the file on the clipboard as `CF_HDROP` (copy, never cut).

### 9.16 `license`
See Section 11.

---

## 10. UI specification

### 10.1 Onboarding (first run)
1. **Welcome + privacy:** "Everything stays on this PC. No account. No tracking." Link to privacy page (local).
2. **Model download:** progress for each stage, speed, pause/resume, "Import model files instead".
3. **Folder picker (shown while downloading):** pre-ticked Documents, Pictures, Desktop, Downloads, Screenshots folder; Videos unticked by default; expandable subfolders to untick; "Add folder…" (native picker). Each row shows file count and estimated time (updates once speed is measured).
4. **Done:** "You can search now with Ctrl+Shift+Space. Files become searchable as they're ready."

### 10.2 Search panel (hotkey)
- Default hotkey **Ctrl+Shift+Space** (configurable; detect conflicts). Avoid Alt+Space (PowerToys Run).
- Frameless, centered, ~680 px wide. Input + result list with thumbnails, file name, folder, snippet/locator ("page 4", "at 12:34").
- Keyboard: ↑/↓ move, Enter open (at page/moment), Space preview, Ctrl+Enter show in Explorer, Ctrl+C copy file, Ctrl+O open big window, Esc close.
- Keyword results appear immediately; semantic results merge in; no layout jumping (reserve rows, animate insertions subtly).

### 10.3 Big window
- Same search with filters (kind, folder, date), image grid view, preview pane with built-in media player and PDF page preview.

### 10.4 Tray
- Status: "Ready" / "Indexing 4,210 of 12,000" / "Paused". Menu: Open search, Pause/Resume, Settings, Quit.

### 10.5 Settings
- **Folders:** add/remove roots, untick subfolders, per-root status.
- **Exclusions:** extra folders/extensions.
- **Performance:** backend (Auto/GPU/CPU), "Only index when plugged in", image quality (Quick/Full refine), CPU threads.
- **Search:** hotkey, default PDF viewer, default media player.
- **License:** status, "Licensed to <email>", activate, deactivate this PC, free-tier usage (x / 300 files).
- **Privacy:** what the app reads, network list (Section 3.3), "Delete index and start over".
- **About:** version, acknowledgements (llama.cpp MIT, EmbeddingGemma 2 Apache 2.0, ffmpeg license, PDFium license, DigUp MIT if code reused), "Check for updates" (manual).

---

## 11. Licensing and payment

### 11.1 Model
- **Free tier:** content search on up to 300 files (all features), plus file-name search on everything. Search panel shows a small "Showing results from 300 of N files — unlock all for $5" note when the limit is reached.
- **Testers/friends:** give a 100% Dodo discount code; they get a normal license key at $0.
- **Paid:** $5 one-time, lifetime license for major version 1, up to **3 PCs**.

### 11.2 Purchase
- Landing page with Dodo Payments checkout (Merchant of Record; handles taxes, supports UPI in India).
- Dodo **License Keys** entitlement attached to the product: auto-generated and emailed on payment; activation limit = 3. VERIFY current Dodo license key docs: https://docs.dodopayments.com/features/license-keys

### 11.3 Activation (one-time, online)
```
App ──POST /activate {license_key, device_id, device_name}──► Activation Worker (Cloudflare)
Worker ──► Dodo public "activate license" endpoint (VERIFY path + payload)
Worker ◄── ok + license info (email, license id) or error (limit reached, invalid, disabled)
Worker signs activation payload with Ed25519 PRIVATE key (Worker secret)
App ◄── activation file (base64url)
App verifies signature with built-in PUBLIC key, saves to license\activation.lic
```

**Activation payload (JSON, then signed):**
```json
{
  "v": 1,
  "product": "findly",
  "license_id": "…",
  "email": "buyer@example.com",
  "device_id": "…",
  "max_major": 1,
  "issued_at": "2026-10-11T00:00:00Z"
}
```
File format: `base64url(payload_json || ed25519_signature)` (signature = last 64 bytes).

**Device ID:** `hex(sha256(MachineGuid + "findly-device-v1"))[:32]`, where MachineGuid is read from `HKLM\SOFTWARE\Microsoft\Cryptography\MachineGuid`. Never send hardware serials.

### 11.4 Verification (every launch, offline)
- Decode → split → `ed25519.Verify(pub, payload, sig)` → parse → check `product`, `device_id == this device`, `max_major >= app major`, license_id not in the compiled-in blocklist.
- Valid → paid mode. Anything else → free tier (L-3). No network call (L-2).

### 11.5 Deactivation
- Settings → "Deactivate this PC" → Worker → Dodo deactivate endpoint → delete local activation file.

### 11.6 Phone activation (later, optional)
- App shows a code containing license key + device_id (or QR). User opens `https://<site>/activate` on a phone, gets back the activation file as a short code to type/paste. For PCs that are never online.

### 11.7 Activation Worker (server/activation-worker)
- TypeScript Cloudflare Worker, endpoints: `POST /activate`, `POST /deactivate`, `GET /health`.
- Secrets: Ed25519 private key, Dodo API settings if needed.
- Rate limiting per IP and per key. No logging of anything beyond what's needed to debug (no IP storage).
- Key generation script (`scripts/gen-license-keys.go`): generates Ed25519 key pair once; public key → `internal/license/pubkey.go`; private key → Worker secret only. **Never commit the private key.**

---

## 12. Performance budgets

| Operation | Target (mid laptop, CPU) | Target (low-end, CPU) |
|---|---|---|
| Keyword results after keystroke | < 30 ms | < 50 ms |
| Query embedding (warm model) | < 80 ms | < 200 ms |
| Binary scan 100k vectors + int8 rerank 200 | < 10 ms | < 25 ms |
| Panel open on hotkey | < 150 ms | < 300 ms |
| New screenshot searchable after save | < 5 s | < 15 s |
| Idle RAM (app only, models unloaded) | < 150 MB | < 150 MB |
| Search-mode RAM (text model loaded) | < 600 MB | < 600 MB |

Measure with `cmd/cli bench` (Section 14.5). Record results in `docs/decisions.md`.

---

## 13. Phases, tasks and acceptance criteria

### Phase 0 — Model spike on Windows (2–3 days)
**Tasks**
1. `scripts/fetch-llama.ps1`: download pinned llama.cpp Windows release (Vulkan + CPU builds); record tag + hashes.
2. Download EmbeddingGemma 2 GGUF parts (text, vision, audio); record sizes + SHA-256 in the manifest.
3. Find out exactly how llama-server serves text, image and audio embeddings for this model (flags, projector files, request JSON). Document in `docs/decisions.md`.
4. `cmd/cli spike`: start llama-server, embed: `"a dog on the beach"` (query prefix), a dog-on-beach photo, a cat photo, a 30 s audio clip of speech about sleep, and `"where they talk about sleep"`.
5. Compute cosine similarities; confirm text↔image and text↔audio pairing works (dog text closer to dog photo than cat photo, etc.).
6. Compare CPU vs Vulkan vectors (cosine > 0.999 for same input). Save CPU vectors as reference fixtures `testdata/reference_vectors.json`.
7. Measure per-item time: text passage, image at 70 and 280 tokens, 30 s audio, on your PC; if possible also on a low-end PC.

**Acceptance**
- Cross-modal similarities behave as expected.
- CPU vs GPU vectors match.
- Timings recorded. Decision recorded on whether DigUp's llama.cpp patch is needed.

### Phase 1 — Text engine as CLI (1 week)
**Tasks**
1. `safefs`, `appfs` with tests (Section 9.1, 9.2, invariants I-1..I-5).
2. `store`: schema, migrations, WAL, cascade deletes incl. FTS.
3. `scan`: skip rules, classification, counts.
4. `extract`: txt, md, html, docx, odt, rtf, pdf (text pages; OCR stub for now).
5. `chunk`, `embed` (text only), `vecfmt`.
6. `index`: pipeline for text kinds with priorities, dedupe, change detection.
7. `search`: keyword + semantic + RRF + exact boost.
8. `cmd/cli`: `index <folder>`, `search "<query>"`, `stats`.
9. `evals/`: `queries.jsonl` format `{"query":"…","expect":["relative/path.pdf#page=3", …]}` with ≥ 25 text queries over `testdata/` + a testbed builder script; `cli eval` prints Recall@5, MRR@10.
10. **File-integrity test** (Section 14.3).

**Acceptance**
- `cli index` + `cli search` work on a real Documents folder.
- Recall@5 ≥ 0.8 on text evals (adjust target after first run, record baseline).
- File-integrity test passes. All unit tests pass.

### Phase 2 — Images + OCR (1 week)
**Tasks**
1. Build `helpers/ocr-helper` (C# Native AOT recommended; Rust acceptable). Protocol per 9.7.
2. `ocr` client in Go with lifecycle via `sidecar`.
3. Image extraction, downscale, embed at 70 tokens; OCR text → FTS (+ passage item if long).
4. Scanned-PDF page OCR path.
5. `thumbs`.
6. Binary + int8 search path with in-memory cache; benchmarks.
7. Add ≥ 15 image/screenshot eval queries (incl. exact-string ones like an invoice number inside a screenshot).

**Acceptance**
- "dog on the beach" finds the photo; an invoice number visible only inside a screenshot is found by exact search.
- Benchmarks meet Section 12 vector-scan budget.
- Evals: record baseline for image queries.

### Phase 3 — The desktop app (1–2 weeks)
**Tasks**
1. Wails app skeleton, React UI, bundled assets only (P-2).
2. `model` staged parallel download with resume, SHA-256, import.
3. Onboarding flow (10.1) with folder picker + estimates.
4. Global hotkey search panel (10.2) with keyword-first + semantic merge, cancellation.
5. Tray (10.4), big window (10.3) basic version, settings (10.5) for folders/performance/privacy/about.
6. `opener` (files, PDF page, Explorer, clipboard copy).
7. Backend auto-selection (Vulkan vs CPU) with reference-vector check.

**Acceptance**
- Fresh Windows user account: install → download → pick folders → search works within minutes.
- Panel opens < 300 ms; typing feels instant.
- Network monitor shows no connections except model download.

### Phase 4 — Keeping the index fresh (3–5 days)
**Tasks**
1. `watch` with debounce, create/modify/delete/rename handling.
2. USN journal catch-up + fallback scan.
3. `power` (battery saver, AC-only option), pause/resume.
4. Deferred jobs: long PDF pages, idle refine pass.

**Acceptance**
- Take a screenshot (Win+Shift+S then save, or Win+PrtScn) → searchable by its text within the Section 12 budget.
- Delete a file → disappears from results immediately and its rows are gone from the DB.
- Files added while the app was closed are picked up on next launch.

### Phase 5 — Licensing + payment (3–5 days)
**Tasks**
1. `scripts/gen-license-keys.go`; `internal/license` verification + free-tier enforcement + blocklist.
2. `server/activation-worker` with `/activate`, `/deactivate`; Dodo integration (VERIFY endpoints); rate limiting.
3. Settings → License UI: activate, deactivate, status.
4. Dodo product + license key entitlement setup (manual steps documented in `docs/release.md`).

**Acceptance**
- Unit tests: valid file passes; edited payload fails; wrong device fails; blocklisted fails; corrupt/missing → free tier without crash.
- End-to-end on Dodo test mode: buy → email key → activate → restart offline → still licensed.
- With network disabled from launch, licensed app works fully (L-2).
- Unlicensed app at 300 files keeps searching existing files and file names (L-3).

### Phase 6 — Audio + video (1 week)
**Tasks**
1. Audio encoder staged download (only if media roots exist); ffmpeg on-demand download with hash check.
2. Audio windows, video scene/interval frames, soundtrack windows (9.6).
3. Built-in preview player at timestamp; external player options.
4. ≥ 10 audio/video eval queries.

**Acceptance**
- "where they talk about sleep" jumps to the right minute of a test recording.
- A video with a distinctive object is found and opens at (approx.) the right moment.

### Phase 7 — Ship (3–5 days)
**Tasks**
1. Installer via `wails build -nsis`; bundle llama-server (Vulkan + CPU), ocr-helper; acknowledgements file.
2. `docs/privacy.md` → also shown in app; README with privacy promises, system requirements, SmartScreen note.
3. Release checklist (`docs/release.md`): run all tests, evals, file-integrity test, offline test on a clean VM, low-end PC smoke test.
4. Landing page + Dodo checkout link.

**Acceptance**
- Release checklist completed on a clean Windows 10 and Windows 11 VM.

### Later (not v1)
- Code search (`code:` prefix, separate index, open in editor at line).
- Microsoft Store listing (MSIX; test sidecars inside MSIX path virtualization).
- Phone activation (11.6). Code signing certificate. 4-bit model option after quality evals.

---

## 14. Testing strategy

### 14.1 Unit tests
Every `internal/` package. Table-driven. Use `testdata/` fixtures (small txt, md, docx, odt, pdf incl. one scanned page, png screenshot with known text, photos, short audio, short video).

### 14.2 Reference-vector test
- Fixtures: CPU-computed vectors for ~10 fixed inputs (text, image, audio).
- On each backend: cosine(new, reference) > 0.999, else that backend is rejected at runtime and the test fails in CI.
- Run after every llama.cpp version bump.

### 14.3 File-integrity test (I-9)
1. Copy `testdata/` to a temp folder; record SHA-256, size, mtime, attributes for every file.
2. Run full indexing, then searches, then watcher events, then "remove folder", then "rebuild index".
3. Re-record; **any difference fails the test.**
4. Also assert: while indexing, the test can rename, edit and delete a file being indexed without errors.

### 14.4 Offline test
- Run the app with networking blocked (Windows Firewall outbound rule for the exe in a VM, or a test hook that fails all dials). Index + search + license verification must work. Any attempted connection other than user-initiated ones fails the test.

### 14.5 Evals and benchmarks
- `cli eval` → Recall@5, MRR@10 per modality; keep results in `evals/results/<date>.json`; don't merge ranking changes that lower scores without a note.
- `cli bench` → per-item embed time per kind, scan time, query latency; used for Section 12.

---

## 15. Packaging and release notes

- Bundle: app exe, `llama-server.exe` + ggml DLLs (Vulkan + CPU variants), `ocr-helper.exe`, acknowledgements/licenses.
- Not bundled: model files (downloaded), ffmpeg (on demand).
- Target installer size < 40 MB; measure and record.
- Unsigned installers trigger SmartScreen; document in README; buy code signing later.
- Never use "Gemma"/"Google" in app name, icon, or domain. "Built on EmbeddingGemma 2 by Google DeepMind" in About/description is fine.

---

## 16. Open questions / VERIFY list

1. Exact llama-server flags and request format for EmbeddingGemma 2 text/image/audio embeddings in the pinned version; whether DigUp's patch is still needed.
2. Exact GGUF files, sizes and hashes (Q8_0) and whether Hugging Face downloads need login; set up a mirror if so.
3. Whether Vulkan backend output matches CPU (fp16 risk). If not, CPU-only or a different GPU path.
4. Wails version: tray + global hotkey + frameless panel support.
5. ncruces/go-sqlite3: FTS5 support and performance; contentless FTS delete semantics.
6. go-pdfium wasm mode: text extraction + page rendering performance.
7. HEIC decoding without cgo (WIC via helper?) or skip in v1.
8. USN journal access rights for a non-admin user; fallback strategy.
9. Dodo license key endpoints (activate/deactivate) exact paths and payloads; payout to Indian bank account; test mode.
10. Windows OCR language availability per user; message for missing language packs.

---

## 17. Conventions

- Go: `gofmt`, `go vet`, `staticcheck`. Errors wrapped with `%w`. Context passed everywhere long-running. No global mutable state except config singletons with mutex.
- Logging: `log/slog` to rotating local file; never log file contents or search queries (paths only at debug level, off by default).
- Commits: one task per commit, message references phase/task (e.g. `P1-T6: index pipeline priorities`).
- Every new package gets tests in the same commit.
- Update `docs/progress.md` after each task and `docs/decisions.md` after each VERIFY item.
