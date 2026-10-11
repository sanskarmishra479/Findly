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
