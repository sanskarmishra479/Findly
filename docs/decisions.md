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
- Open: pooling flag, image token budget control (70 vs 280), whether DigUp's patch is still needed, CPU vs Vulkan vector match.
