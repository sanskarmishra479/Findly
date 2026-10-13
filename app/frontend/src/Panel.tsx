import { useEffect, useRef, useState } from "react";
import { HidePanel, IndexStatus, Info } from "../wailsjs/go/main/App";
import { EventsOn } from "../wailsjs/runtime/runtime";

// The hotkey search panel (plan 10.2). Results arrive in T4.
//
// Closing on a click elsewhere is handled in Go: Windows reports when another window
// comes to the front, which is more reliable than the page's focus events.
export function Panel() {
  const [query, setQuery] = useState("");
  const [warning, setWarning] = useState("");
  const [indexError, setIndexError] = useState("");
  const [status, setStatus] = useState("");
  const input = useRef<HTMLInputElement>(null);

  useEffect(() => {
    // Startup runs in parallel with the page: every value may also arrive later as an
    // event, so an answer from Info never clears what an event already set.
    Info().then((i) => {
      if (i.hotkeyWarning) setWarning(i.hotkeyWarning);
      if (i.indexError) setIndexError(i.indexError);
    });
    IndexStatus().then((s) => {
      if (s) setStatus((cur) => cur || s);
    });
    const offWarning = EventsOn("hotkey:warning", (w: string) => setWarning(w));
    const offIndexError = EventsOn("engine:error", (m: string) => setIndexError(m));
    const offStatus = EventsOn("engine:status", (s: string) => setStatus(s));
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
      offIndexError();
      offStatus();
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
        {indexError && <p className="warning">{indexError}</p>}
        {warning && <p className="warning">{warning}</p>}
        <p className="hint">Results will appear here.</p>
      </div>
      <footer>
        <span>Enter open · Esc close</span>
        <span className="status">{status || "Findly"}</span>
      </footer>
    </div>
  );
}
