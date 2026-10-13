# Progress

## Setup
- [x] Git repo + GitHub remote
- [x] Go module `github.com/sanskarmishra479/findly`

## Phase 0 — Model spike
- [x] T1 `scripts/fetch-llama.ps1` (pinned b11552, CPU + Vulkan, SHA-256 verified)
- [x] T2 `scripts/fetch-model.ps1` (Q8_0 text + mmproj, SHA-256 verified)
- [x] T3 llama-server flags + request formats for text / image / audio (docs/decisions.md)
- [x] T4 `cli spike`
- [x] T5 Cross-modal similarity check: all pass
- [x] T6 CPU vs Vulkan: worst cosine 0.99982; `testdata/reference_vectors.json` saved; `cli compare`
- [x] T7 Timings on dev PC (i5-12450H + RTX 2050)
- [ ] T7b Timings on a low-end PC (optional, when one is available)

## Phase 1 — Text engine CLI
- [x] T1 `safefs` (read-only, share flags, cloud-only + link skip, long paths) + `appfs` (os.Root, atomic writes) with tests
- [x] T1b Go toolchain 1.27.2 required in go.mod; govulncheck clean
- [x] T2 `store` (SQLite schema, migrations, cascade deletes incl. FTS, vectors, free-tier count)
- [x] T3 `scan` (skip rules, secrets, code projects, excludes, counts)
- [x] T4 `extract` — txt, md, csv, log, json, html, docx, odt, rtf + PDF (go-pdfium wasm, sandboxed)
- [x] T5 `chunk`, `embed`, `vecfmt` (+ `sidecar`)
- [x] T6 `index` (pipeline, priorities, dedupe, deletions, free tier)
- [x] T7 `search` (keyword + semantic + RRF + exact boost, filters)
- [x] T8 CLI index/search/stats/remove (+ `check`, `reset`; `-kind`, `-folder`, `-after`, `-before`)
- [x] T9 evals (28-file testbed, 46 queries incl. paraphrases, multi-answer and "should find nothing"; `cli eval` with cutoff/gap sweep). Chosen cutoff 0.64 + gap 0.06 (Vulkan, 2026-10-12): Recall@5 = 1.000, MRR@10 = 0.976, 3 of 4 "should find nothing" queries clean ("asdfgh qwerty" still returns results); results in `evals/results/`
- [x] T10 file-integrity test (`go test ./internal/integrity`)
- [x] Full Phase 1 review: all High and Medium findings fixed and re-reviewed (see docs/decisions.md)
- [x] Low findings
- [x] Acceptance test on a real Documents folder (2026-10-12, see docs/decisions.md)

**Phase 1 complete.**

## Notes for later phases
- Phase 4: deferred job for partial PDFs (pages after 30); needs a `last_page` column (new migration).
- Existing llama.cpp installs have no `.verified` marker yet: the next `scripts\fetch-llama.ps1` run downloads them once more (≈53 MB).

## Phase 3 — Desktop app (built before Phase 2)
- [x] T1 Wails v2 skeleton: `app/` + `app/frontend/`, pinned deps, WebView data in the app folder, single instance, CSP, starts hidden
- [x] T2 Tray icon + global hotkey (+ T1 review fixes)
- [x] Decided: accept WebView2's own connection to Microsoft at start, documented in plan O-1 (revisit with Wails v3)
- [ ] T3 Engine service (background indexing, model lifetime, keyword-first indexing)
- [ ] T4 Search panel (keyword-first + meaning merge, keyboard navigation)
- [ ] T5 Opener (open, PDF at page, show in Explorer, copy file)
- [ ] T6 Model download + onboarding
- [ ] T7 Settings + basic big window
- [ ] T8 Backend auto-selection (Vulkan vs CPU)
- [ ] Acceptance
