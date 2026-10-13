# Search panel design (P3-T4)

User references (2026-10-13): `panel-bar.png` (closed state) and `panel-results.png` (with results). They were drawn on a Mac; Findly is the Windows version.

## States
1. **Bar only.** Ctrl+Shift+Space shows a single search box and nothing else: no empty results area.
2. **Results.** As soon as the user types and something matches, the window grows downward into two columns. Results that arrive later never make the layout jump.

## Look
- Translucent "glass" background (Windows 11 acrylic) with rounded corners and a soft shadow.
- **Follows the Windows light/dark setting** (user decision).
- Accent colour for the search icon, the selected row and the Open button.
- **Position:** horizontally centered, in the upper third of the screen (user decision), so the bar stays put when it grows downward.

## Results column (left)
- Grouped by kind with small headings (DOCUMENTS, IMAGES, ...). Best match first within the list.
- Each row: thumbnail, file name (cut with "…"), page badge ("p. 1") when there is a page, and a one-line snippet with the query words in bold.
- The selected row is highlighted; ↑/↓ move the selection.

## Preview column (right)
- **PDF:** a picture of the matching page, rendered by the PDF engine Findly already uses and cached, with the matched words highlighted and a page badge (user decision). **Images:** the image itself. **Word / text files:** the matching passage as formatted text with highlights.
- File name, folder (shortened with `~` for the user folder), then "PDF · date · pages · size".
- Why it matched: **"Has your words"** (keyword match, snippet with bold words) and/or **"Matches meaning"** (semantic match).
- Buttons: **Open** (primary) and **Show in Explorer**.

## Footer
- Left: what is happening ("Matching words…", "Matching meaning…", indexing status).
- Right: shortcuts. Windows equivalents of the reference: **Enter** Open · **Ctrl+Enter** Show in Explorer · **Ctrl+C** Copy file · **Esc** Close.

## Behaviour
- Clicking a result, or pressing Enter, opens the file in its normal app; a PDF opens at the matching page (T5 opener).
- Keyword results appear while typing; meaning results merge in when the model answers, without layout jumps (plan 10.2).
- Only the actions from plan I-6: open, open at page, show in Explorer, copy. Never delete, move or rename.
