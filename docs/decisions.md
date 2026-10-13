# Decisions

Record of what was decided and why. Newest at the bottom of each section.

## Naming
- **2026-10-11** DisplayName `Findly`, AppID `findly` (AppID frozen after first release). Module path `github.com/sanskarmishra479/findly`.

## Pricing
- **2026-10-11** Free tier = content indexing for 300 files + file-name search on everything. Testers get 100% Dodo discount codes (no special license path).

## Model + llama.cpp (Phase 0, VERIFY items 1–2)
- **2026-10-11** llama.cpp pinned to **b11552** (2026-10-10). EmbeddingGemma 2 support landed in ggml-org/llama.cpp PR #30054 (merged 2026-10-06, first in ~b11474). Builds used: `win-cpu-x64` and `win-vulkan-x64`. Hashes in `scripts/fetch-llama.ps1`.
- **2026-10-11** Model files from `ggml-org/embeddinggemma-2-GGUF` (official GGUF repo linked from the PR):

  | File | Size | SHA-256 |
  |---|---|---|
  | `embeddinggemma-2-Q8_0.gguf` (text) | 309,855,456 | `2188ac1d…b09135` |
  | `mmproj-embeddinggemma-2-Q8_0.gguf` (vision + audio) | 554,821,024 | `c4a8a526…0aab64f` |

  Full hashes in `scripts/fetch-model.ps1`.
- **2026-10-11 — plan change:** the GGUF repo ships vision **and** audio encoders in **one** mmproj file (555 MB Q8_0). The plan's staged "vision then audio" download is not possible with these files. Stages become: (1) text model 310 MB, (2) mmproj 555 MB. Total ≈ 865 MB. Revisit if a split mmproj appears or if we build our own split.
- **2026-10-11** Server flags (from community reports, to confirm in spike): `llama-server --model <text.gguf> --mmproj <mmproj.gguf> --embeddings`. Text-only search mode can omit `--mmproj`.
- **2026-10-11** Multimodal embedding request shape (from llama.cpp issue #30082, closed as invalid — the reporter's 400 was a request-shape error):
  ```json
  {"model":"embeddinggemma-2",
   "input":[{"content":[{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,..."}}]}]}
  ```
  on `POST /v1/embeddings`. Do **not** use `/embedding` with `image_data`: it silently ignores the image and returns a text-only vector. Audio part type still to confirm (likely `input_audio`).
- **2026-10-11 — llama-server hardening** (from `--help` of b11552). Always start with:
  `--host 127.0.0.1 --port <random> --offline --no-mmproj-auto --no-webui`, API key via the `LLAMA_API_KEY` **env var** (not argv, so it is not visible in process listings).
  Start the process with a **clean environment** (only `SystemRoot`, `PATH`, `LLAMA_API_KEY`): llama-server reads `LLAMA_ARG_*` and `HF_TOKEN` from env, and has experimental `--tools` (read_file, grep…) and an MCP proxy. A stray env var must never turn those on. Never pass `--tools`, `--media-path`, `-hf`, `--embd-gemma-default`.
- **2026-10-11** `--ubatch-size` must cover the whole input for embeddings (non-causal); 2048 for the spike (30 s audio ≈ 750 tokens). Image budget flags: `--image-min-tokens` / `--image-max-tokens`.
- **2026-10-11 — dependency policy:** stable, maintained, security-checked libraries only; prefer stdlib and `golang.org/x`. Every dependency is checked (release status, maintenance, license, `govulncheck`) and recorded here. Global hotkey: use `RegisterHotKey` via `golang.org/x/sys/windows` instead of `golang.design/x/hotkey`. Wails v3 stability to be checked before Phase 3.

## Phase 0 results (2026-10-11)
Test PC: Intel i5-12450H (8 cores / 12 threads), NVIDIA RTX 2050 (Vulkan), llama.cpp b11552, Q8_0 text + Q8_0 mmproj, 256-d vectors.

- **Cross-modal works.** Text↔image and text↔audio pairing pass at every image budget:

  | Pair | 70 tok | 280 tok | 676 tok (server default) |
  |---|---|---|---|
  | "a dog on the beach" vs dog photo / cat photo | 0.781 / 0.660 | 0.764 / 0.619 | 0.744 / 0.621 |
  | "a cat sitting outside" vs cat / dog | 0.773 / 0.633 | 0.739 / 0.597 | 0.735 / 0.581 |
  | "where they talk about sleep" vs audio / dog text vs audio | 0.798 / 0.560 | same | same |

- **Image budget must be set explicitly.** Without flags, llama-server sizes images by resolution (1536×1024 → 676 tokens, 24 s each on CPU), not the model's 280 default. Use `--image-min-tokens N --image-max-tokens N`. First pass = **70** (actual 64 tokens).
- **CPU vs Vulkan match:** worst cosine **0.99982** (≥ 0.999 required). Vulkan accepted on this PC. `cli compare` implements the check; CPU vectors saved as `testdata/reference_vectors.json` (image budget 70).
- **Timings** (warm; per item, one request each, includes decoding a 2.4 MB PNG):

  | Item | CPU | Vulkan (RTX 2050) |
  |---|---|---|
  | Query text (~14 tokens) | 30–45 ms | 15–20 ms |
  | Image, 70 tokens | 1.5–1.6 s | 0.4–0.5 s |
  | Image, 280 tokens | 6.7 s | not measured |
  | Audio clip (837 tokens) | 5.7 s | 0.8 s |

- **Vulkan first run:** first image and first audio take ~50–60 s (shader compile). The driver caches it, so later launches are fast. The app must show "preparing GPU…" on first run and not mistake this for a hang.
- **DigUp's llama.cpp patch is not needed:** upstream b11552 handles text, image and audio embeddings without patches.
- llama-server warns that **audio input is experimental** in llama.cpp ("may have reduced quality"). Track before Phase 6.
- Query embedding meets the Section 12 budget on CPU (< 80 ms).
- Not yet done: timings on a low-end PC. Open: explicit `--pooling` (model default works; leave unset).

## Phase 1
- **2026-10-11 — dependency check: `golang.org/x/sys` v0.49.0.** Maintained by the Go team, BSD-3 license, used for raw Windows file APIs (`CreateFile`, `FindFirstFile`). No govulncheck findings reached by our code.
- **2026-10-11 — `appfs` uses `os.Root`** (stdlib, Go 1.24+) to confine all writes to `%LOCALAPPDATA%\Findly`. Tests confirm writes and deletes through a junction pointing outside are refused, and removing the junction removes only the link.
- **2026-10-11 — Go 1.27.0 has 9 stdlib vulnerabilities** (govulncheck), fixed in **1.27.2**. One is directly relevant: GO-2026-6604 "Root.Mkdir(All) can follow junctions out of the root on Windows". Our junction tests do not reproduce it on 1.27.0. **Done:** go.mod now requires `go 1.27.2` (Go auto-downloads and checksum-verifies that toolchain); govulncheck reports no vulnerabilities.
- **2026-10-11 — safefs reparse rule:** only *name-surrogate* reparse points (symlinks, junctions, mount points) are treated as links and skipped. Other reparse points (downloaded OneDrive files, WOF-compressed system files, dedup) are normal files. `Open` opens with `FILE_FLAG_OPEN_REPARSE_POINT` first so a file swapped for a link after the check is still refused.
- **Known gap:** the symlink-file test is skipped unless Developer Mode is on (creating symlinks needs that privilege); the junction test covers directory links.
- **2026-10-11 — P1-T1 review fixes** (independent review agent):
  - `safefs` now checks the **real path of every handle** (`GetFinalPathNameByHandle`) and refuses it if any folder in the path is a junction/symlink (confirmed bug: `root\jump\secret.txt` was readable). Directories are checked the same way before listing. New `safefs.Canonical(path)` resolves a folder's real path once, when the user adds a root; roots are stored in that form. Side effect: paths using 8.3 short names are refused (we only use long names).
  - Reopen of non-link reparse points keeps the first handle open and proves both handles are the same file (volume serial + file index); cloud-only is checked before reopening.
  - `WalkDir`: SkipDir from an error callback continues with siblings (was ending the walk); SkipDir on a file skips the rest of the folder (io/fs semantics); cloud-only folders are not listed (listing can trigger OneDrive network fetches).
  - `ReadFile` rejects negative / MaxInt64 limits (overflow returned empty data).
  - `appfs.Path()` refuses existing links along the path; `appfs` refuses Windows device names with extensions (`NUL.txt`, `COM1.db`).
  - **Accepted residual risks:** (a) a link swapped in between `checkDir` and `FindFirstFile` during a walk, or after `appfs.Path()` returns; exploiting it needs a malicious local process that can already modify the user's folders, and the worst outcome is Findly *reading* or indexing an extra folder. (b) Holding a file open blocks renaming its **parent folder** (no share flag avoids this): keep read handles short-lived. (c) The reopen branch only runs for downloaded OneDrive files (Windows hides the WOF-compression reparse flag even on handles); it cannot be tested without OneDrive.
- **2026-10-11 — SQLite library: `github.com/ncruces/go-sqlite3` v0.35.6** (user's choice, as in the plan). Check: MIT; actively maintained (release 2026-09-23); no entries in the Go vulnerability database; govulncheck clean. Trade-off accepted: **v0 = pre-1.0 API** (updates may need code changes; pin and test before bumping). Plus: SQLite is translated from Wasm (`wasm2go`) and runs sandboxed, so a SQLite memory bug cannot corrupt the rest of the app. Dependencies it adds: `ncruces/go-sqlite3-wasm/v6` (SQLite 3.53.x translation, MIT-0 + SQLite public domain), `ncruces/julianday` v1.0.0. No wazero any more. Alternative considered: `modernc.org/sqlite` v1.60.1.
- **2026-10-11 — store design:**
  - DSN form `file:C:/…/index.db?_pragma=…` (the Go VFS rejects `file:///C:/…`). Pragmas: WAL, synchronous NORMAL, foreign_keys ON, busy_timeout 10 s; `_txlock=immediate`.
  - Schema version lives in `PRAGMA user_version` (not `meta`), so it is updated atomically with the migration. Opening a DB from a newer app version fails instead of corrupting it.
  - FTS5 table uses `contentless_delete=1`, and an `AFTER DELETE ON items` trigger removes keyword rows. Cascading deletes (root → files → items) fire the trigger, so a deleted file is never searchable (P-5, tested).
  - Every file gets one `kind='name'` item whose FTS row holds the file name (with `_ - .` turned into spaces); content items have an empty name column.
  - Paths are `COLLATE NOCASE` (Windows is case-insensitive).
  - Free tier counts files with status `done` or `partial` (`CountContentIndexed`).
- **2026-10-11 — scan:** own recursion over the exported `safefs.ReadDir` (not `WalkDir`) so the scanner sees a folder's full listing before descending; needed to detect code projects (`.git`, `package.json`, `go.mod`, `Cargo.toml`, `pom.xml`), inside which only images are indexed. Unsupported files are still emitted as kind `other` (name-only search, plan 8). Secret-looking files (P-6) are skipped completely, not even name-indexed; added `id_ecdsa*`/`id_dsa*` to the plan's list. The "images smaller than 128×128" rule needs image headers, so it moves to `extract`. Unreadable folders are counted in `Stats.Errors` and skipped.
- **2026-10-11 — P1-T3 review fixes (scan):**
  - A root that is missing, unplugged, a link, cloud-only or not a folder returns `ErrBadRoot` — never "0 files". Otherwise an unplugged USB drive would look empty and its whole index would be purged.
  - **Secret rules widened (P-6), decided by Claude, user can change:** folder names are checked too (`Passwords\`, `Secrets\`, `credentials\` are not entered). Added `credential`, `passwd`, `.ppk .jks .keystore .gpg .asc`, `.netrc`, `.git-credentials`, `.pgpass`, `accesskeys.csv`. Narrowed: `.env` / `.env.*` only (not `.envelope.txt`); `secret` does not match `secretary`/`secretariat`.
  - Only the HIDDEN attribute skips entries; SYSTEM alone does not (a visible folder can get SYSTEM from a `desktop.ini` customization). Checked on the dev PC: Documents/Downloads/OneDrive have no SYSTEM attribute.
  - OneDrive folders are reparse points with tag `0x9000701A` (cloud, not a name surrogate) → treated as normal folders. Verified on the dev PC (Pictures, Desktop, Documents live under OneDrive).
  - Excludes are canonicalized (8.3 names, `\?\`, trailing `\`, case, `/`) and compared with `EqualFold`; drive-root excludes work.
  - `safefs` clamps FILETIME conversion (a 1601 timestamp overflowed into the future).
  - Counts only include files the callback accepted; cancellation checked every 256 entries.
- **2026-10-11 — P1-T2 review fixes (store):**
  - **"database is locked" bug (confirmed by reviewer, 438/480 ops failed under load):** `busy_timeout` is now the first pragma (the driver applies them in order). WAL is set once in `Open` (it is stored in the file), with a 10 s retry on BUSY. Test: 8 workers × 40 ops with a new connection per op, plus a second `Open` at the same time: 0 failures. The app must still run as a single instance (Phase 3).
  - `DeleteFile` / `RemoveRoot` return thumbnail paths (collected in the same transaction) so the caller can delete the image files (P-5).
  - `UpsertFile` always refreshes root, path/name casing, ext and kind; a renamed file gets a new file-name search row.
  - `AddRoot` refuses nested roots (`ErrNestedRoot`): every file belongs to exactly one root.
  - Free tier counts files that **have content items**, so a modified file keeps its slot while waiting; `SetFileResult(failed|skipped)` removes stale content.
  - `AUTOINCREMENT` on roots/files/items: ids are never reused (protects the future in-memory vector cache). Migration 1 edited in place because it has never shipped.
  - Paths are matched by a `path_key` column (Go `strings.ToUpper`), since SQLite `NOCASE` only folds ASCII (`Ärger.txt` vs `ärger.txt`).
  - `Open` refuses UNC and `\?\` paths with a clear error (the index always lives under `%LOCALAPPDATA%`).
- **2026-10-11 — extract (non-PDF):** txt/md/csv/log/json, html/htm, docx, odt, rtf. Only new dependency: **`golang.org/x/net` v0.61.0** (Go team, BSD-3; `html` tokenizer only; govulncheck clean). Everything else is stdlib (`archive/zip`, `encoding/xml`, `unicode/utf16`).
  - Encodings: UTF-8 (± BOM), UTF-16 LE/BE with BOM, else Windows-1252 (own 32-entry table, no `x/text` dependency).
  - Untrusted-input limits: text files 20 MB, JSON 5 MB, docx/odt 200 MB on disk, 100 MB uncompressed XML part (checked from the zip header **and** enforced with a LimitReader, so a lying header can't bypass it), 10 MB extracted text per file. Zip-bomb tests included. `encoding/xml` never resolves external entities (no XXE). RTF nesting capped at 1000 groups.
  - HTML: visible text + `<title>`; script/style/noscript/template/svg/iframe/object dropped.
  - `MostlyNumeric` (> 60 % digits/punctuation) for plan 9.3's "FTS only, no embedding" rule; applied by the indexer.
  - Test-writing note: the file-writing tool turned `荤` inside a Go raw string into a literal character; such fixtures are written as `\` + `u8364` concatenations.
- **2026-10-11 — P1-T4 review fixes (extract):** all confirmed findings fixed with regression tests that bound time and memory:
  - **DoS fixes:** repeated `<title>` was quadratic (8 MB file: 3.5 min, 305 GB allocated) → only the first title, capped at 1 KB. XML nesting capped at 512 levels, and per-token work is now constant (counters, not stack scans). Zips with > 10,000 entries (or ZIP64) are refused by reading the end-of-directory record before Go builds its directory. `tidy` walks lines with `strings.Cut` (20 MB of newlines allocated 340 MB). RTF decodes bytes by table lookup and skips picture data cheaply.
  - **Correctness:** `<svg/>`-style self-closing tags no longer hide the rest of an HTML page; table cells / dt / dd / header / footer etc. are separated. DOCX: `mc:Fallback` skipped (text boxes appeared twice), text boxes on their own line, paragraph tab stops no longer emitted as tabs. ODT: tracked-change deletions and comments (with author/date) are not indexed; footnotes on their own line.
  - **Encodings:** UTF-8 with a few broken bytes stays UTF-8 (`multibyte >= invalid`) instead of turning the whole file into mojibake; UTF-16 without BOM detected from the zero-byte pattern.
  - **RTF:** `\ucN` clamped to 0..8; fallback counter reset at `{`/`}`; lone surrogates become U+FFFD without losing the next character; `\binN` data skipped.
  - **Known limits (accepted for v1):** HTML `<meta charset>` is ignored (non-UTF-8, non-1252 pages may be garbled); RTF `\ansicpg` is ignored (modern Word writes `\uN`, which works); DOCX/ODT headers, footers, footnotes (docx) and comments are not read.
- **2026-10-11 — PDF: `github.com/klippa-app/go-pdfium` v1.21.3 (WebAssembly mode)**, user-approved. Compiled-in deps: `tetratelabs/wazero` v1.12.0, `google/uuid` v1.6.0, `jolestar/go-commons-pool/v2` v2.1.2 (last updated 2023, small), `golang.org/x/text`. The module's beta deps (wago, wazy, grpc) are only for other modes and are not compiled in. govulncheck clean.
  - **Sandbox:** the library's default mounts the whole current drive into PDFium; we pass an **empty `wazero.FSConfig`** and give PDFium bytes read via `safefs` (test proves it cannot open `C:\…` or `/Windows/win.ini`). Stdout/stderr discarded; memory capped at 512 MiB (`WithMemoryLimitPages(8192)`); `WithCloseOnContextDone` + `Kill()` after a 2-minute timeout.
  - First pass extracts pages 1–30 (`Doc.MorePages` marks the rest for a deferred job via `PDFPages(path, from, to)`); cap 5,000 pages; files over 256 MB refused. Pages with < 20 characters of text are listed in `Doc.NeedsOCR` for Phase 2. Password-protected PDFs return `ErrPDFPassword` and are not opened.
- **2026-10-11 — T5 vecfmt/chunk/embed/sidecar:**
  - `vecfmt`: bit i of byte i/8 = sign of dimension i; int8 scale = max|v|/127. Benchmarks on dev PC: Hamming scan of 100k vectors **1.07 ms** (budget 5 ms), int8 re-rank of 200 **0.15 ms**. NaN/Inf/zero/short vectors are rejected (`ErrBadVector`), catching broken GPU backends.
  - `chunk`: 1,400-rune target (~350 tokens), 200-rune overlap (~50 tokens), 2,000-rune hard max; cut at paragraphs, then sentences (incl. 。！？), then spaces, then plain rune counts (CJK/base64). Passages never cross PDF pages. 10 MB of text chunks in 0.36 s.
  - `sidecar` (new package, plan 6): children get only the env we pass (+SystemRoot), `CREATE_NO_WINDOW`, optional `BELOW_NORMAL_PRIORITY_CLASS`, and are placed in a **kill-on-close job object** — tested: a child dies when its parent process exits. `Tail` keeps the last N bytes of output for error messages (no log files from helpers).
  - `embed`: model-card prefixes; batches of 16; LRU cache of 256 queries; queries cut at 500 runes; HTTP client with `Proxy: nil`; responses capped at 64 MB and checked (count, index, NaN). Restarts a crashed server up to 3 times (tested by killing it). Real-model test: three queries each matched the right passage.
- **2026-10-11 — P1-T4 PDF review fixes:** consecutive page errors (≥ 3) now fail the call instead of silently skipping the rest (reviewer killed the engine mid-file: 227 of 3000 pages came back with no error); single bad pages go to `Doc.FailedPages`. Waiting for a free engine (`pdfWait` 5 min → `ErrPDFBusy`, caller should retry) is separate from the 2-minute extraction timeout (`ErrPDFTimeout`). Password/encryption detected with go-pdfium's typed `ErrPassword`/`ErrSecurity`. **MaxPDFFile lowered to 128 MB and one engine at a time** (peak ≈ 640 MB instead of ~2 GB). The page that crosses the text cap is cut, not dropped; `Doc.LastPage` tells the deferred job where to continue, and `MorePages` is false once the per-file text cap is reached. `ClosePDF` is mutex-guarded and re-usable. Verified by reviewer: empty `FSConfig` mounts nothing, no temp files, no socket imports, wazero clamps memory to 512 MiB, and `Kill()` frees the pool slot. Engine start costs ~5 s once (WebAssembly compile).
- **2026-10-11 — T6 index pipeline:** scan → extract workers (half the cores) → single embed+store stage. Priority: modified < 30 days, then Desktop/Documents/Downloads, then text/doc/pdf. Change detection via `UpsertFile` (size+mtime; pending and skipped files are retried). Files missing from a **complete** scan (no unreadable folders, root available) are deleted from the index (P-5); an unplugged root (`scan.ErrBadRoot`) deletes nothing. Dedupe: sha256 of the extracted text (no second file read); identical content is copied (`store.CopyContent`), not re-embedded. Mostly-numeric passages are keyword-only. Free tier: 300 files with content, counted atomically across the feeder and store stages; beyond it files are `skipped` without being read, and are picked up after purchase. An embedding-server failure stops the run (`ErrEmbedder`) instead of marking every file failed; `ErrPDFBusy` leaves the file pending. Kind `other` is marked done (name-only). Images/audio/video stay pending for Phases 2/6.
- **2026-10-11 — P1-T5 review fixes:**
  - **P-4 port squatting (confirmed by reviewer):** between choosing a free port and llama-server binding it, another program could take the port and receive the API key and query text. Now, before *any* request (even `/health`), `GetExtendedTcpTable` must show our child's PID as the listener; otherwise the process is stopped and a new port is tried (3 tries). Tested with a fake server: it received 0 requests.
  - **Token sizes:** chunk sizes are now UTF-8 **bytes** (separators counted; passages are at most `Target` by construction, and the tests check them against `Max` 2,000). Measured: rare scripts use ~1 token per byte (byte fallback), so 2,000 bytes always fits the 2,048-token batch. A rejected batch is retried passage by passage; a too-long passage is cut using the token count from the server's error message.
  - **Errors split:** `embed.ErrServer` (crash, 5xx, bad/NaN output, wrong dimension → the indexer stops) vs `embed.ErrInput` (this input only → file failed). A cancelled caller gets `context.Canceled`.
  - **Restarts:** startup runs on the server's own lifetime context, never a caller's (a cancelled search no longer kills startup or burns a restart); the counter resets after any successful request (max 3 *consecutive* crashes); old processes are stopped before relaunch; `Close` does not wait behind a startup.
  - **Memory:** `--ctx-size 4096 --parallel 2` → llama-server 299 MB working set / 437 MB private (was 664 / 758 MB; budget 600 MB). `--no-webui` → `--no-ui` (deprecated).
  - `sidecar.Stop` terminates the whole job (grandchildren too) and `cmd.WaitDelay` = 2 s; `Tail` drops oversized buffers. Embeddings must be exactly 768-d. `vecfmt.ToInt8` returns an empty vector for values too small to scale. Synthetic recall test: binary top-200 recall@10 = 1.00 (real-model recall is measured in evals).
  - Accepted: a child could start grandchildren before it is assigned to the job object (llama-server starts none).
- **2026-10-11 — P1-T6 review fixes (index):**
  - **L-3 bug (confirmed):** editing an already-indexed file while at the 300-file limit marked it `skipped`, which deleted its content. Jobs now carry `hadContent`; such files always keep their slot, and re-indexing never counts them twice (second confirmed bug: 10 wasted slots).
  - Only `embed.ErrInput` fails a file; any other embedder error (crash, reset, timeout, NaN) stops the run with `ErrEmbedder` and leaves files pending. Failed files are retried after 24 h (`store.RetryFailedAfter`; failures like a locked file are often temporary).
  - Deleted-file cleanup skips only folders that could not be read this run (`scan.Stats.FailedDirs`, up to 1,000), instead of being turned off for the whole root.
  - Progress: `Total` counts only files this phase processes; images/audio/video stay pending without being queued; `other` files are marked done during the scan. Feeder skips flow through the results channel (one goroutine owns progress). The feeder is in the WaitGroup so `results` never closes while it can still send.
  - Workers check for cancellation before starting an extraction.
  - Dedupe lookup + copy are one transaction (`store.CopyDuplicate`), so a source deleted in between can't produce an empty "done" copy. Known: copied vectors keep the source file's name as embedding title.
  - **Phase 4 (recorded):** partial PDFs (`status=partial`) are not finished yet; the deferred job and saving `LastPage` belong to Phase 4 task 4.
- **2026-10-11 — T7 search:**
  - **Query safety:** user text is never passed to FTS5 as syntax. `search.Parse` keeps only letter/digit runs (exactly how the `unicode61` tokenizer splits), every word is double-quoted, and words are joined with AND; the last word gets `*` while typing. `"quoted parts"` become required phrases. Tested with FTS5 operators, column filters, NUL, SQL-looking text and 1,000-word queries.
  - Keyword: bm25 with the file-name column weighted 2×, best item per file. Semantic: `VectorCache` (all vectors in RAM) → Hamming top-200 with a max-heap → int8 re-rank → best item per file; hits below cosine 0.25 are dropped. Benchmark: **100k vectors in 1.4 ms** (budget 10 ms).
  - Fusion: RRF k=60 plus a +0.05 boost when the query words appear verbatim (punctuation-insensitive: `INV-2024-0042` = `inv 2024 0042`) in the file name or snippet. Semantic-only hits are dropped when the query has a quoted phrase they don't contain.
  - Filters (kind, folder, date) run in SQL for keyword hits and on file rows for semantic hits; folder filter uses `substr(path_key)` (no LIKE, so `%`/`_` in paths are safe) and requires a whole-folder prefix.
  - Deleted files never appear even with a stale vector cache (results are joined to current file rows). If the embedder is unavailable, keyword results are still returned with the error.
- **2026-10-11 — T8 CLI:** `cli index <folder>`, `cli search "<query>"` (`-keyword`, `-kind`, `-folder`, `-n`), `cli stats`, `cli remove <folder>`; shared flags `-data` (default `%LOCALAPPDATA%\Findly`), `-backend cpu|vulkan`, `-third-party`. Ctrl+C cancels indexing. Removed folders/files also delete their thumbnail files through `appfs`. **`-unlimited` is a developer-only flag** that ignores the 300-file limit; acceptable because the CLI is a dev tool and is not bundled in the installer (Phase 7 ships only the app) — must be re-checked at release.
  - Smoke test (4 files, 42 passages): every query put the right file first ("can I keep a pet" → lease, "trouble sleeping" → sleep notes, "INV-2024-0042" → error page via words+meaning+exact); searches 40–57 ms including query embedding. Indexing: **CPU 38 s vs Vulkan 3.6 s** (after the one-time ~50 s shader compile) → the app should prefer Vulkan when the reference-vector check passes (Phase 3 task 7).
  - Seen in smoke test: unrelated files still appear as weak "meaning" results (cutoff 0.25 too low for this model) → to be calibrated with the evals (T9).
  - `search.snippet` finds words with a case-insensitive regexp on the original text (ToLower changes byte lengths for e.g. "İ", which could misplace the cut).
- **2026-10-12 — P1-T7/T8 review fixes (search + CLI):**
  - **Keyword speed** (50k passages, `BenchmarkKeyword50k`): FTS5 `prefix='2 3'` index; ranking inside an FTS subquery before joining; **while typing, unranked first matches** (common word 2.1 ms, 2-letter prefix 2.6 ms — was 272 ms / 2.1 s); the ranked search (70–100 ms for very common words) runs after the typing pause together with the semantic path, as in plan 9.11. Prefix `*` only from the 2nd letter. Queries capped at 500 runes / 32 words (a 50,000-word paste took 7.4 s).
  - One hit per file in SQL results, so one big file no longer hides others.
  - **Semantic cutoff:** absolute 0.60 **plus relative 0.08 below the best hit**. Reviewer measured with the real model: right pairs 0.73–0.85, unrelated 0.51–0.68, gibberish 0.61–0.66. Eval (2026-10-12, Vulkan): Recall@5 = 1.000, MRR@10 = 1.000 at every setting tried; unrelated results per query: 0.60 alone 5.2 → 0.60+0.08 **1.4**; 0.65 kept as headroom only. Results in `evals/results/2026-10-12-vulkan.json`.
  - Filters are applied **during** the vector scan (cache keeps path/kind/mtime per file), not after the top 200.
  - Quoted phrases and the exact-match boost compare whole, punctuation-normalized words (`"inv 2024"` matches `INV-2024`; "cat" no longer boosts "concatenate").
  - CLI: thumbnail paths from the DB must be `filepath.IsLocal` before deletion (a corrupt `..\license\x` could otherwise delete app files); Ctrl+C works during model start; the model is started before a folder is added; `-folder`/`remove` accept relative paths, `C:` and trailing `\`; `-kind` values are trimmed.
  - Phase 3 notes: the app needs incremental vector-cache updates after indexing, and search should respect `roots.enabled`.
- **2026-10-12 — T10 file-integrity test** (`internal/integrity`, plan 14.3 / I-9): builds a folder from the eval corpus (real PDF/DOCX/ODT) plus a read-only file, a hidden file, an empty file, Unicode names, a > 260-character path, an "offline" (cloud-only) file, a secret file, a code project, real images, and a junction to an outside folder. Records SHA-256, size, mtime and attributes of every file and folder (using `os`, not `safefs`), then runs index ×2, six searches, remove folder, **delete + rebuild the index**, and all of that again. Any difference fails. A second test edits/renames/deletes 300 files continuously while three indexing runs happen: the "user" program must never get an error, and indexing must not fail. `TestSnapshotDetectsChanges` proves the check notices content, mtime, attribute, deletion and creation changes. Not covered yet: watcher events (Phase 4) and last-access time (NTFS may update it on any read; not part of the plan's check).
- **2026-10-12 — T9/T10 review fixes:**
  - **Integrity test hole (confirmed by reviewer):** returning `SkipDir` for the junction made `WalkDir` skip every later entry of that folder, so the snapshot covered only 26 of 61 entries. Fixed; the test now asserts specific late entries are present, and additionally checks: no file handle is left open after indexing (exclusive open of every file), the secret/hidden/cloud-only files are not in the index, and user edits really overlapped each indexing run. Findly passes all of it.
  - **Keyword search:** ranked path uses an adaptive window (best 1,000 passages by bm25, grouped per file in Go; grows 8× when fewer than 50 files were found and more matches exist, judged against the total match count), so one big file or a narrow filter can't hide other files. Typing path streams matches with no window and stops at 50 files. 50k passages: typing 0.9–1.5 ms, ranked common word 74 ms, rare word 1.6 ms. Folder filter uses `length(?)` (characters) — `len()` in Go counted bytes and broke non-ASCII folders.
  - **Harder evals:** 46 queries (added paraphrases without shared words, multi-answer queries, 4 "should find nothing" queries) and 2 distractor documents. **Cutoff recalibrated to 0.64 absolute + 0.06 relative:** Recall@5 = 1.000, MRR@10 = 0.976, 0.6 unrelated results per query, 3 of 4 nonsense queries return nothing (gibberish "asdfgh qwerty" still returns one weak hit). 0.66 starts missing paraphrases; 0.60 let 3 of 4 nonsense queries return files.
  - Query embedding on this laptop: **CPU 40 ms vs Vulkan 77 ms** for a single short query (GPU wake-up/transfer overhead), while Vulkan is ~10× faster for indexing batches → Phase 3: search-mode server on CPU, indexing-mode server on GPU (fits plan section 5's two modes).
  - Generated ODT files follow the spec (stored `mimetype` first, manifest). `embed.Start` stops a server that finished starting just as the caller cancelled.

## Phase 1 full review (2026-10-12) — High fixes
Three parallel review agents covered the whole phase; 8 High findings, all confirmed by running code, fixed with regression tests:
1. **Zip entry cap bypass:** the end-of-directory count was trusted; Go's reader ignores it. `zipEntryCount` now walks the central-directory headers itself and refuses > 10,000 (a forged 186 MB .docx with 1.9 M entries used 508 MB).
2. **PDF goroutine panic crashed the app:** `recover()` in the extraction goroutine → `ErrPDFCrash` (instance killed, never reused); negative text budgets refused; `cutUTF8` clamps `n <= 0`.
3. **PDF memory:** the file is read only *after* an engine is free, so at most one PDF is in memory (measured 979 MB with 6 waiting workers before). Test hooks `readPDF`/`pdfTextFn`.
4. **Secrets (P-6):** skip `.docker`, `.password-store`, `.terraform` folders; `.kdb .psafe3 .1pux .1pif .agilekeychain .opvault .tfstate .tfvars`; `logins.csv/json`, `key4.db`, `wallet.dat`, `.npmrc`, `.pypirc`; names containing bitwarden/lastpass/1password/keepass/dashlane/nordpass.
5. **Path keys:** `safefs.PathKey` upper-cases with Windows' `RtlUpcaseUnicodeString` (what NTFS uses). `strings.ToUpper` merged σ/ς, µ/μ, s/ſ, i/ı, which NTFS keeps as different files → shared one index row. Used by store, index and scan excludes.
6. **Damaged index (I-7):** `PRAGMA quick_check(1)` at open → `store.ErrCorrupt` / `IsCorrupt`; `store.DatabaseFiles`; `cli reset` deletes the index + thumbnails through appfs. The CLI also refuses an empty `-data` (it silently used the current folder — found when a failed command created app folders in the repo, since removed).
7. **GPU driver writes outside the data folder:** the NVIDIA driver created `NVIDIA Corporation\umdlogs` in llama-server's working folder. llama-server now runs with working folder **and** LOCALAPPDATA/APPDATA/TEMP/TMP set to `appfs.DirGPU` (`%LOCALAPPDATA%\Findly\gpu`); `embed.Config.WorkDir` is required. Verified with a Vulkan run: nothing appears next to the exe or in the repo. Stray empty folders from earlier runs removed.
8. **Crash loop:** `embed.ErrCrashed` (with `ErrServer`) when llama-server exits during a request. The indexer retries the file once after the automatic restart; on a second crash only that file is marked failed and indexing continues; 3 crashing files in a row stop the run (the server, not the files, is broken). Cancellation during embedding now returns `context.Canceled` (was reported as "embedding server failed").
- Tooling: staticcheck 2026.2.1 cannot read Go 1.27 export data yet (re-check when a new release ships); `deadcode` found only `VectorCache.Len`.
- **2026-10-12 — review of the High fixes, follow-ups:**
  - Zip: the stored directory **offset** was still forgeable (Go's reader falls back to end-record − size); now offset + size must equal the end-record position, and `dirSize == 0xFFFFFFFF` (ZIP64) is refused.
  - Crash handling: restarts caused by a crashing *input* no longer count toward the 3-restart limit (two bad files used to leave the server dead for the whole session, also for search); `crashed()` checks the exact process that served the request (a parallel restart hid crashes). Tested with a fake llama-server (`internal/embed/testdata/fakeserver`) that exits on "poison": after 6 crashes, indexing and search still work.
  - `quick_check` moved out of `Open` (it took ~10 s on a 217 MB index at every start) into `Store.Verify` (app: background/idle; CLI: `cli check`). Errors met during normal use with `IsCorrupt` → the CLI prints the reset hint. `IsCorrupt` now uses SQLite error codes only (text matching caught "fts5: malformed MATCH expression").
  - Filtered vector scan: path keys computed once at cache load, per-search filter is a `[]bool` per file (not a map), ASCII fast path in `PathKey`: 100k files 371 ms → **4.8 ms**.
  - `PathKey` everywhere paths are compared (index failed-folder guard, `cli remove`, `safefs` final-path check).
  - More secret names: `.p8`, `.keychain-db`, `Login Data`, `.vault-token`, `rclone.conf`, `.s3cfg`, recovery/backup codes, mnemonic / seed-phrase files. `cli spike` removes its temporary GPU folder.
  - Dev databases created before this change used `ToUpper` path keys: run `cli reset` once (no release yet).
- **2026-10-12 — verification of round 2, follow-ups:** all round-2 fixes verified by running code (zip offsets, crash restarts, `Verify`: Open 11 ms vs Verify 8.2 s on a 210 MB index, filtered scan 3.8–5.5 ms at 100k, PathKey everywhere). Further fixes:
  - Zip: instead of requiring offset + size = end record (which refused valid small files that carry a ZIP64 end record/locator), the entry count is now taken at **both** places Go's reader may read the directory from (stored offset, and end − size), and the larger count is used. No stored position is trusted.
  - Crash streak: input crashes still don't use the 3-restart budget, but **5 crashes in a row with no successful request** mean the server itself is broken; it then stays stopped (checked before any restart path). Tested with the fake server.
  - Secrets: `mnemonic` only counts together with seed/wallet/phrase/crypto words ("Mnemonics for Biology.pdf" was skipped); recovery/backup-code names match with spaces, `-` or `_`.
  - `cli`: the damaged-index hint is printed once.
  - Accepted (low impact): when one request's input crashes the server, a concurrent request (e.g. a search during indexing) can also get `ErrCrashed`; indexing uses one embed worker, and a search just retries.

## Phase 1 full review — Medium fixes (2026-10-12)
- **Hindi/Indic search (#9):** `search.words` keeps combining marks inside words (Devanagari vowel signs are marks); "किराया" was split into letters joined by AND and matched almost any Hindi text. Tested: SQLite's tokenizer already matches a whole quoted word precisely, so no index change was needed; accented Latin is unaffected.
- **Cancel ≠ server failure (#10):** done with the High fixes.
- **Skipped files (#11):** `UpsertFile` no longer re-offers unchanged free-tier-skipped files on every scan (a write per file per rescan); `store.SkippedFiles` + `index.queueSkipped` queue them only when a license or free slots exist.
- **CLI (#12):** `cli index` accepts relative folders; flags after the query are an error (Go's flag parsing stops at the first non-flag, so `-n 1` became search words); `-n < 1` and unknown `-kind` values are refused.
- **Phrases / exact boost (#13):** checked on the full passage text (not the 200-char snippet), with whole, normalized words; quoted parts also earn the exact boost (plan 9.11).
- **Model version (#14):** `store.EnsureModel` writes `model_version` (`embed.ModelVersion`), `vector_dim`, `created_at` to meta on first use and returns `ErrModelChanged` for a different model; the CLI checks it whenever the model starts.
- **Timeout (#15):** per-request timeout 1 min + 1 min per input (was a fixed 5 min for 16 passages).
- **Secret roots (#16):** `scan.Root` refuses a chosen folder that is, or is inside, a credentials folder (`.ssh`, `.aws`, …) or a folder named with the words password(s)/secret(s)/credential(s) (`ErrSecretRoot`). Whole words only for path parts (substring rules hit e.g. "TestScanSkipsSecrets123"); folders inside AppData remain allowed.
- **Read-only type (#17):** `safefs.Open` returns `*safefs.File` (Read/ReadAt/Seek/Stat/Name/Close only), so code that writes to a user file does not compile.

## Phase 1 review — Medium fixes, round 2 (2026-10-12)
- **Emoji:** a combining mark only counts as part of a word after a letter or digit, and variation selectors never do. `"rent ❤️"` had become `"rent" AND "️"` and found nothing.
- **Spaceless scripts:** quoted phrases in Chinese, Japanese, Thai, Lao, Khmer or Myanmar text are checked as substrings, because whole-word checks never match inside a sentence with no spaces.
- **Model info:** `EnsureModel` writes `vector_dim`, `created_at` and `model_version` in one transaction (`model_version` last), so a crash can't leave an index that looks like it came from another model.
- **Secret roots:** `index.Indexer.AddRoot` refuses credential folders *before* saving them (a saved one failed every run and blocked adding its parent). The chosen folder itself also gets the full subfolder name rules (`KeePass`, `MyPasswords`, `passwd`); its parent folders keep the narrower whole-word rules.
- **Free slots:** `queueSkipped` subtracts this scan's new files from the free room first.
- **CLI:** the flags-after-query hint keeps the whole query and the flag values; words like `-5` are search words unless they name a real flag.
- Accepted: `SkippedFiles` picks the newest skipped files and ignores the Desktop/Documents preference (only affects which files fill a few freed slots).

## Phase 1 review — Medium fixes, round 3 (2026-10-12)
- **Phrase ends:** `containsWords` matches a phrase as a substring, but each end must touch a space unless a letter on either side of that end is from a spaceless script. `"cat 猫"` no longer matches "concat 猫"; `"東京"` still matches "東京に行きます".
- **Secret folders:** a new `IsSecretFolderName` covers folders (no extension rule, so "Website.asc" and "Bob.Key" are fine folders), used for subfolders during scans and for the chosen folder. Parent folders get every rule except the bare "secret" substring, which only counts as a whole word there, so `D:\KeePass\exports` is refused while Go test temp folders like "TestScanSkipsSecrets123" are not. UNC share names (`\server\Passwords`) are checked too.
- `Indexer.AddRoot` checks the canonical path (".", 8.3 short names); the CLI checks before starting the model.
- `EnsureModel` reads and writes in one write transaction, so the first app to record a model wins.
- Accepted: names containing "secret" ("secretsauce recipes") are still refused as chosen folders and subfolders, because safety comes first. Freed free-tier slots taken by new files that then fail stay unused until the next scan.

## Phase 1 full review — Low fixes (2026-10-12)
- **Fetch scripts:** `fetch-llama.ps1` extracts into `<variant>.partial`, writes a `.verified` marker holding the zip's SHA-256, then swaps the folder in; installs without a matching marker are fetched again. `fetch-model.ps1` re-hashes an existing model instead of trusting its size. The Hugging Face URL stays on `resolve/main`: the pinned hashes make an upstream change fail safely.
- **CLI:** `search -after/-before YYYY-MM-DD` (local days, plan 9.11). `spike -data` was renamed `-media`, because `-data` means the app data folder everywhere else. `compare` refuses vectors of different lengths (it used to panic).
- **Cleanup:** chunk comments now say bytes, not characters, and `Max` is documented as a test bound. The unused `sidecar.ErrExited` is removed. `store.Count` checks `rows.Err()`. The `Indexer.store` method is renamed `save`, because it shadowed the package name.
- **Accepted:** `staticcheck` doesn't support Go 1.27 yet, so `go vet` and `govulncheck` are used until a release does. The spike tool has no port-owner check (dev-only; the app path in `embed` has one).

## Phase 1 review — Medium round 4 + Low part 2 (2026-10-12)
- **Secret folders:** every part of a chosen path gets the full folder rules, so `MySecrets\Bank` is refused, just as a scan of the parent skips `MySecrets`. The parent exception existed only for Go test temp folders, so the tests were renamed ("Secret" became "Private") instead.
- Accepted: parents such as `PasswordReset\src` or `Credentialing Committee\Minutes` are refused. This matches what a scan of their parent already skips; safety comes first.
- **Secret names:** trailing dots and spaces are ignored (`key.pem.`, `id_rsa `).
- **Phrases:** ー/ｰ (Common script, used only in kana words) and Korean (particles attach to words) count as spaceless.
- **`EnsureModel`:** reads first without a lock and takes the write transaction only on first use.
- **Cleanup:** removed the unused `VectorCache.Len`.
- Accepted (scope): password managers with other names ("Vault", "Enpass") and non-English words ("Passwort", "Contraseñas") are not detected. `\?\C:\…` roots get a "not available" error instead of a clearer one.

## Phase 1 review — final review fixes (2026-10-12)
- **fetch-llama.ps1:** the folders are swapped by renaming (`cpu` becomes `cpu.old`, then `cpu.partial` becomes `cpu`). Renaming works while llama-server.exe is running; deleting doesn't, and a failed delete used to leave a broken install. The leftover `.old` is removed on the next run. An empty `.verified` marker just triggers a reinstall. Tested with a fake zip and a locked exe.
- **Korean phrases:** a phrase may be followed by more Hangul (particles) but must not start inside a Hangul word, so `"울"` no longer matches "서울에서".
- **Parent folders:** "secret" must be a word of its own (`My Secrets`, `MySecrets`, `old_secret`), so `C:\Users\secretagent` and "Secretion studies" don't block everything below them. The chosen folder itself keeps the substring rule.
- **Date filters:** `dayStart` moves a missing local midnight (DST zones that jump at 00:00) to the first real moment of the day. 1970-01-01 UTC no longer turns into "no filter".
- **`cli compare`:** real cosine (divides by the norms), and a reference with no vectors is refused.
- **Follow-up (narrow review):**
  - fetch-llama.ps1 renames the old install back if moving the new one into place fails (e.g. antivirus holding a new file open). Tested with a locked file.
  - `dayStart` gives up after one day of steps, so a day a zone skipped entirely (Samoa 2011-12-30) is an error instead of an endless loop.
  - Parent folder names with no lower-case letters ("MYSECRETS", "TOPSECRET") use the substring rule, because case shows no word boundaries there.
  - Accepted: an all-lower-case parent like `mysecrets` is allowed as a parent, although a scan of its own parent would skip it. This is the price of not blocking user names like "secretagent".

## Phase 1 acceptance (2026-10-12)
Real folder: `C:\Users\sansk\OneDrive\Documents` (user's OK), Vulkan, separate test data folder (deleted afterwards because it held document text).
- **Index:** 17 files. 7 cloud-only files were skipped (not downloaded), desktop.ini was skipped, and 9 were found (7 PDFs, 1 docx, 1 png pending for Phase 2). 8 files indexed, 0 failed, 357 passages, 2 min 14 s. All 7 PDFs are over 30 pages, so they are `partial` (the remaining pages are the Phase 4 deferred job).
- **Search:** meaning queries ("work experience and skills" found the resume first; "what technologies were used to build the system" found the architecture page) and keyword queries ("abstract": words+meaning+exact) worked in 87–103 ms with the model warm.
- **Integrity:** the metadata snapshot (size, modified, created, attributes, for 18 entries) was identical before and after; cloud-only files stayed cloud-only. Nothing was written outside the data folder.
- **Evals:** Recall@5 1.000 (target ≥ 0.8), MRR@10 0.976. All unit tests and the file-integrity test pass.
- **Found for later phases:**
  - Duplicate copies (5 identical reports) fill the result list; the UI should group identical files (same content hash).
  - "Conclusion and future scope" found the table of contents, because the real conclusion lies past page 30 (deferred pages).
  - 2 min 14 s for ~150 unique passages seems slow; profile PDF extraction versus embedding before Phase 3.

## Indexing speed investigation (2026-10-12)
- `index.Stats.Timing` and `cli index` now report the time spent on model start, extraction (summed over workers), embedding (with the passage count), and saving, plus the time spent waiting for extraction.
- On the real Documents folder (Vulkan, warm): **21.5 s total** = model start ~7 s + embedding ~12.5 s (307 passages, ~40 ms each) + waiting for the first extraction ~9 s. Database writes ~0.2 s. The first run took 2 min 14 s and a second run 1 min 4 s (embedding 60 s for 357 passages); the difference is one-time GPU/driver warm-up and cold file caches, not Findly code.
- **Bug fixed:** long PDFs (`partial`, first 30 pages) were never deduplicated, because `contentHash` returned "" for them and `CopyDuplicate` required `done`. Now partial files get a hash, and a copy is only made between files with the **same status**. `ss.pdf` (a true copy) is no longer embedded twice. The five "Major Project Report" files are different versions (sizes differ), not copies.

## Phase 3 — T1 app skeleton (2026-10-13)
- **Order and model policy (user decisions):** Phase 3 comes before Phase 2. The search model is never kept loaded permanently. It unloads after ~2 min idle, and keyword results cover the ~4 s reload. Both are recorded in plan.md.
- **Wails v2.16.0** (stable). v3 is still a beta (see docs/phase3-prep.md). Tray and hotkey (T2) are built on x/sys/windows.
- **Layout:** `app/` is the Wails project root (wails.json, main package), with the UI in `app/frontend/`. The plan had `frontend/` at the repository root, but `//go:embed` cannot reach a parent folder. One Go module for everything; the Wails CLI finds `go.mod` in a parent folder.
- **Window:** frameless 680×460 panel, always on top, not resizable, starts hidden; closing only hides it. Single-instance lock: a second start shows the running app's panel.
- **Focus:** a second start calls `AllowSetForegroundWindow(ASFW_ANY)` before Wails hands over to the running copy, so the panel may come to the front. "Click elsewhere closes the panel" only applies once Windows has really made the panel the active window (`GetForegroundWindow` belongs to our process). Otherwise a refused focus change (background launcher, foreground lock) made the panel vanish 74 ms after showing (measured).
- **I-3:** WebView2's browser data is written to `%LOCALAPPDATA%\Findly\webview` (`appfs.DirWebview`), not `%APPDATA%\Findly.exe` (verified after a real run).
- **P-1/P-2:** no SmartScreen URL checks (`EnableFraudulentWebsiteDetection: false`); the UI is embedded in the exe; the production page carries a strict Content-Security-Policy (`default-src 'self'`, no inline scripts or styles, no remote anything); system font only; devtools are off in production builds.
- **npm supply chain:** `.npmrc` sets `ignore-scripts=true`, `save-exact=true`, `engine-strict=true` and the public registry only. Exact pins: react/react-dom 19.3.0, vite 8.3.4, @vitejs/plugin-react 6.1.2, typescript 7.0.2, @types/react(-dom) 19.3.0. 24 packages, `npm audit`: 0 vulnerabilities. Every lock entry resolves to registry.npmjs.org.
- **Go dependencies added:** wails/v2 v2.16.0, go-webview2 v1.0.22, wails mimetype v1.4.1 (+ small helpers). `govulncheck`: no vulnerabilities.
- **Placeholder icon:** a white magnifying glass on blue (`app/build/appicon.png`), drawn with a standard-library-only Go program; to be replaced by a real logo.
- **Build:** `cd app; wails build -trimpath` produces `app/build/bin/Findly.exe` (11.5 MB). The manifest uses per-monitor-v2 DPI awareness and runs without admin rights.

## Phase 3 — T1 review fixes + T2 tray and hotkey (2026-10-13)
- **`internal/desktop`** (new): the tray icon (Shell_NotifyIconW; menu: Open search, Quit Findly), the global hotkey (RegisterHotKey, Ctrl+Shift+Space, MOD_NOREPEAT) and a foreground hook (SetWinEventHook EVENT_SYSTEM_FOREGROUND, out of context: nothing is injected into other programs). It is all one hidden top-level window on one locked OS thread with its own message loop. It is a top-level window rather than a message-only one, so it receives TaskbarCreated and re-adds the icon after Explorer restarts. Callbacks run on goroutines, never on that thread. A hotkey owned by another program gives `ErrHotkeyInUse`; the tray keeps working, and the panel and tooltip say so. Tests use Ctrl+Shift+Alt+F24 and posted messages, so they never send real keystrokes.
- **Closing the panel:** decided in Go from Windows foreground events, not from page focus/blur events. The review saw the old way fail 4 of ~14 times, and once hide a panel 1 s after a second start. Rules (`panelState`, unit-tested): another program's window coming to the front closes the panel; events stamped before the panel was shown are ignored (wrap-safe tick comparison); **Esc** hides and gives focus back to the window the user was in. Esc is listened for on `window`, so it works wherever focus is in the page.
- **Second start:** `AllowSetForegroundWindow` now names only the running Findly's PID (found by the Wails single-instance window), instead of `ASFW_ANY` for every process.
- **I-3:** WebView2's temporary `<GUID>.tmp` files went to `%TEMP%`. `TEMP`/`TMP` now point to `%LOCALAPPDATA%\Findly\tmp` (`appfs.DirTmp`) before `wails.Run`. Verified: no new `.tmp` files in `%TEMP%`.
- **Data race:** the Wails context is stored atomically (a second start can call `ShowPanel` before `startup` runs).
- **`run()` before `wails.Run`** stays minimal. It also runs for every second start and for `wails build`'s bindings step, so the index and other heavy setup go in `App.startup` (T3).
- The CSP `<meta>` now comes after `<meta charset>`.
- **Decided — WebView2 network connection (O-1):** accepted and documented (user decision, 2026-10-13); see plan.md O-1. The WebView2 runtime's browser process opens one HTTPS connection to Microsoft (2603:1046:…::443, no reverse DNS) about 1 s after start. Findly code does not cause it. `--disable-background-networking --disable-component-update --disable-domain-reliability --no-pings --disable-client-side-phishing-detection --disable-sync` (tested through a patched copy of Wails) did **not** stop it, and Wails v2 offers no way to pass browser arguments.
- Accepted: `keepDistPlaceholder` writes `.gitkeep` in `closeBundle`, so a failed Vite build can leave dist without it and make `go build` fail until the next good build.

## Phase 3 — T2 review fixes (2026-10-13)
- **TEMP leak:** `TEMP`/`TMP` point to `Findly\tmp` only until the page is up (`OnDomReady`), then the user's values come back. Programs Findly opens later (Word, Edge, Explorer via ShellExecute) inherit the environment and must not put their temp or recovery files in the app folder. WebView2's browser process copied the environment at start, and its `.tmp` files still land in `Findly\tmp` (verified).
- **Registry clutter from tests:** Windows 11 permanently records every program that showed a tray icon (`HKCU\Control Panel\NotifyIconSettings`, visible in Settings > Taskbar). Tests now use `Options.NoTrayIcon`, so `go test` adds no entries and needs no running Explorer.
- **Outside WM_CLOSE:** `Close()` uses a private message (`WM_APP+3`); a WM_CLOSE from another program is ignored. Before, it silently removed the tray, the hotkey and the foreground hook while Findly kept running.
- **Thread:** the desktop goroutine keeps its OS thread locked until it exits, so Go ends the thread instead of reusing one that may hold a queued WM_QUIT.
- **Tray icon is not required at start:** a failed `NIM_ADD` (e.g. at logon before the taskbar exists, or a Shell_NotifyIcon timeout) is no longer fatal; `TaskbarCreated` adds the icon later. `Start` fails only if the window can't be created.
- **Tooltip:** NUL characters are dropped and the 127-unit cut never splits a surrogate pair. The old test passed 500 NULs, which never exercised the cut; it now does.
- **Races:** `desktop`/`hotkeyWarning` are guarded by a mutex. Startup runs in parallel with the page, so the warning is also sent as an event, and the page never clears it.
- **Panel:** an event in the same tick as showing counts as stale (the tick moves in ~16 ms steps, and the tray click that opened the panel can share it). Esc does not return focus to the taskbar or tray overflow (`Window.IsShell`). Esc during IME composition only cancels the composition.
- `Close` twice and `SetTooltip` after `Close` do nothing. The tray icon is loaded with `LR_SHARED` (Windows owns the handle). The comments now say which calls run on which thread.
- **Accepted:**
  - `WindowShow` is asynchronous (posted to the UI thread), while `WindowHide` is direct. A show and a hide very close together can end with the window and the state disagreeing; the next hotkey fixes it.
  - `Window.Ours` compares process IDs, so WebView2 pop-ups (another process) would count as "another program". Check this in T4 if the panel ever opens pop-ups.
- **Verification follow-up:**
  - Only `Close()` can end the desktop. It sets a `closing` flag first; the private close message, WM_DESTROY and WM_QUIT are ignored without the flag. Any program can post them to a top-level window, and before this fix each one left Findly running but unreachable (tested against the real exe).
  - `Close` always removes the tray icon (unless `NoTrayIcon`), even when the add had reported a timeout, so no ghost icon is left.
  - A failed tray-icon add is retried every 10 s (`SetTimer`), not only on Explorer's TaskbarCreated. Otherwise, with the hotkey also taken, Findly would be unreachable without any message.
  - TEMP is also restored at the end of `startup`: the WebView2 browser process exists by then, so the restore no longer depends on the page finishing loading.
  - Esc also ignores `keyCode 229` (IMEs that do not set `isComposing`).
- **Last T2 review (2d1690b):**
  - The message loop ends only after Close's own cleanup has destroyed the window (`destroyed`, set right after `DestroyWindow`). An outside WM_QUIT racing with Close used to end the loop first, leaving the hotkey registered, a ghost tray icon and the hook installed. The new test `TestCloseRacingOutsideQuit` fails on the old code.
  - `Close` retries its PostMessage if the queue is full, so it can't wait forever.
  - When adding the tray icon fails, a modify is tried. If it works the icon already exists and the retry timer stops, instead of retrying every 10 s for good.
  - Flaky test fixed: `TestHotkeyInUse` now waits until its helper has released the hotkey, and tests close a Desktop that `Start` returned together with an error. Before, one failure left a Desktop running and broke every later test.

## Phase 3 — T3a keyword first (2026-10-13)
- **Two steps instead of one.** `IndexRoot` now stores passages and keyword rows without calling the model, so files are keyword-searchable as soon as they are read. `Indexer.EmbedPending` (new) then adds vectors to every waiting passage, file by file in the order they were stored (the indexing priority order). It can be interrupted, and simply continues next time; vectors don't need a rescan.
- **Schema migration 2:** `items.embed` (0 = nothing to do, 1 = waiting for its vector, 2 = the model failed on it), with a partial index `items_embed_pending ON items(id) WHERE embed = 1`. New store functions: `NextPendingFile`, `PendingItems`, `SetVectors`, `MarkEmbedFailed`, `CountPending`. `SetVectors` skips passages that were replaced or deleted meanwhile, so a slow embedding never brings back old content.
- **Model failures no longer fail the file:**
  - A file the model crashes on twice, or rejects as bad input, keeps its keyword search; only its meaning search is given up (`embed = 2`, reason in `files.error`).
  - 3 crashing files in a row stop the pass (the server is broken), and a server error or timeout stops it with the passages left waiting. That is the same protection as before, now in the embedding pass.
- **Dedupe:**
  - When a file is stored, an identical file's content is copied, with its vectors or still waiting.
  - The embedding pass copies vectors only from an identical file whose passages *all* have vectors (`CopyDuplicate(..., finishedOnly)`), never a waiting or failed state; otherwise one failure would silently spread to every copy.
- **CLI `index`:** checks the model files and the index's model first (no empty folder entry when files are missing), then starts the model **in parallel** with reading the files. Measured on the 28-file test set: keyword search ready in < 0.05 s, meaning search after 7.2 s (model start 5 s in parallel, embedding 2.2 s).
- **Evals unchanged:** Recall@5 1.000, MRR@10 0.976, negatives clean 0.75 at cutoff 0.64 / gap 0.06 (Vulkan).
