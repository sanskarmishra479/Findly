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
- [ ] not started
