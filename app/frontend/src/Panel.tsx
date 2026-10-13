import { useEffect, useRef, useState, type KeyboardEvent } from "react";
import { HidePanel, PanelBlurred, PanelFocused } from "../wailsjs/go/main/App";
import { EventsOn } from "../wailsjs/runtime/runtime";

// The hotkey search panel (plan 10.2). T1: the shell only; results arrive in T4.
export function Panel() {
  const [query, setQuery] = useState("");
  const input = useRef<HTMLInputElement>(null);

  useEffect(() => {
    // Each time the panel is shown, start from the input with the old text selected,
    // so typing replaces it.
    const off = EventsOn("panel:shown", () => {
      input.current?.focus();
      input.current?.select();
    });
    // Clicking anywhere else closes the panel, like other launchers. Go decides, because
    // only Windows knows whether the panel really was the active window.
    const onBlur = () => PanelBlurred();
    const onFocus = () => PanelFocused();
    window.addEventListener("focus", onFocus);
    window.addEventListener("blur", onBlur);
    return () => {
      off();
      window.removeEventListener("blur", onBlur);
      window.removeEventListener("focus", onFocus);
    };
  }, []);

  function onKeyDown(e: KeyboardEvent) {
    if (e.key === "Escape") {
      e.preventDefault();
      HidePanel();
    }
  }

  return (
    <div className="panel" onKeyDown={onKeyDown}>
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
        <p className="hint">Results will appear here.</p>
      </div>
      <footer>
        <span>Enter open · Esc close</span>
        <span>Findly</span>
      </footer>
    </div>
  );
}
