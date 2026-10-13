import { useEffect, useRef, useState } from "react";
import { HidePanel, Info } from "../wailsjs/go/main/App";
import { EventsOn } from "../wailsjs/runtime/runtime";

// The hotkey search panel (plan 10.2). Results arrive in T4.
//
// Closing on a click elsewhere is handled in Go: Windows reports when another window
// comes to the front, which is more reliable than the page's focus events.
export function Panel() {
  const [query, setQuery] = useState("");
  const [warning, setWarning] = useState("");
  const input = useRef<HTMLInputElement>(null);

  useEffect(() => {
    Info().then((i) => {
      if (i.hotkeyWarning) setWarning(i.hotkeyWarning); // never clear a warning the event set
    });
    // The warning can be set after the page asked: startup runs in parallel.
    const offWarning = EventsOn("hotkey:warning", (w: string) => setWarning(w));
    // Each time the panel is shown, start from the input with the old text selected,
    // so typing replaces it.
    const off = EventsOn("panel:shown", () => {
      input.current?.focus();
      input.current?.select();
    });
    // On the window, not an element: Esc must work wherever focus is in the page.
    const onKey = (e: KeyboardEvent) => {
      // During IME composition (Japanese, Chinese, Hindi input...) Esc cancels the
      // composition, not the panel.
      // (Some IMEs report the key as keyCode 229 instead of setting isComposing.)
      if (e.key === "Escape" && !e.isComposing && e.keyCode !== 229) {
        e.preventDefault();
        HidePanel();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => {
      off();
      offWarning();
      window.removeEventListener("keydown", onKey);
    };
  }, []);

  return (
    <div className="panel">
      <div className="search">
        <svg className="search-icon" viewBox="0 0 24 24" aria-hidden="true">
          <circle cx="10.5" cy="10.5" r="6.5" />
          <line x1="15.5" y1="15.5" x2="21" y2="21" />
        </svg>
        <input
          ref={input}
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder="Search your files by words or meaning"
          spellCheck={false}
          autoFocus
          aria-label="Search"
        />
      </div>
      <div className="results">
        {warning && <p className="warning">{warning}</p>}
        <p className="hint">Results will appear here.</p>
      </div>
      <footer>
        <span>Enter open · Esc close</span>
        <span>Findly</span>
      </footer>
    </div>
  );
}
