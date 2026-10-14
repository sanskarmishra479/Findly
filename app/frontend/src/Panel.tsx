import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  CopyResult,
  HidePanel,
  IndexStatus,
  Info,
  OpenResult,
  Preview,
  Search,
  SetExpanded,
  ShowResultInExplorer,
} from "../wailsjs/go/main/App";
import { main } from "../wailsjs/go/models";
import { EventsOn } from "../wailsjs/runtime/runtime";
import { FileIcon } from "./FileIcon";
import { Highlight } from "./Highlight";
import { describe, groupOf } from "./format";

type Result = main.SearchResult;
type PreviewData = main.PreviewResponse;

// Meaning results need the model (tens of milliseconds, or seconds to load it): ask once
// the user pauses typing. Word results are asked on every keystroke.
const MEANING_DELAY = 300;
const PREVIEW_DELAY = 120;

// The search panel (docs/design/search-panel.md). Closing on a click elsewhere is done
// in Go: Windows reports when another window comes to the front.
export function Panel() {
  const [query, setQuery] = useState("");
  const [results, setResults] = useState<Result[]>([]);
  const [words, setWords] = useState<string[]>([]);
  const [busy, setBusy] = useState<"" | "words" | "meaning">("");
  const [selected, setSelected] = useState<number>(0); // result id
  const [preview, setPreview] = useState<PreviewData | null>(null);
  const [notice, setNotice] = useState(""); // short message, e.g. "Copied"
  const [warning, setWarning] = useState("");
  const [indexError, setIndexError] = useState("");
  const [meaningOff, setMeaningOff] = useState("");
  const [status, setStatus] = useState("");
  const input = useRef<HTMLInputElement>(null);
  const seq = useRef(0);
  const previewSeq = useRef(0);
  const meaningTimer = useRef<number>(0);

  // Groups in the order of their best result; results ranked within each group. The
  // keyboard moves through them in this displayed order.
  const groups = useMemo(() => {
    const out: { label: string; items: Result[] }[] = [];
    for (const r of results) {
      const label = groupOf(r.kind);
      let g = out.find((x) => x.label === label);
      if (!g) out.push((g = { label, items: [] }));
      g.items.push(r);
    }
    return out;
  }, [results]);
  const ordered = useMemo(() => groups.flatMap((g) => g.items), [groups]);
  const current = ordered.find((r) => r.id === selected) ?? ordered[0];

  // Keep the selection on the same file when results change (no jumping); else the top.
  const show = useCallback((rs: Result[]) => {
    setResults(rs);
    setSelected((id) => (rs.some((r) => r.id === id) ? id : (rs[0]?.id ?? 0)));
  }, []);

  const runSearch = useCallback(
    (q: string) => {
      const s = ++seq.current;
      window.clearTimeout(meaningTimer.current);
      if (q.trim() === "") {
        show([]);
        setWords([]);
        setBusy("");
        SetExpanded(false);
        return;
      }
      SetExpanded(true);
      setBusy("words");
      Search(s, q, false).then((r) => {
        if (r.seq !== seq.current) return; // an older query
        show(r.results ?? []);
        setWords(r.words ?? []);
        setBusy((b) => (b === "words" ? "meaning" : b));
      });
      meaningTimer.current = window.setTimeout(() => {
        setBusy("meaning");
        Search(s, q, true).then((r) => {
          if (r.seq !== seq.current) return;
          show(r.results ?? []);
          setMeaningOff(r.meaningOff ?? "");
          setBusy("");
        });
      }, MEANING_DELAY);
    },
    [show],
  );

  // Preview of the selected result, a moment after the selection settles.
  useEffect(() => {
    if (!current) {
      setPreview(null);
      return;
    }
    const s = ++previewSeq.current;
    const t = window.setTimeout(() => {
      Preview(s, current.id, current.itemId, query).then((p) => {
        if (p.seq === previewSeq.current) setPreview(p);
      });
    }, PREVIEW_DELAY);
    return () => window.clearTimeout(t);
    // The preview follows the selected file and passage, not every keystroke.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [current?.id, current?.itemId]);

  const flash = (msg: string) => {
    setNotice(msg);
    window.setTimeout(() => setNotice(""), 1800);
  };
  const open = (r?: Result) => r && OpenResult(r.id, r.page).then((m) => m && flash(m));
  const reveal = (r?: Result) => r && ShowResultInExplorer(r.id).then((m) => m && flash(m));
  const copy = (r?: Result) => r && CopyResult(r.id).then((m) => flash(m || "Copied"));

  useEffect(() => {
    // Startup runs in parallel with the page: values may also arrive later as events,
    // so an answer from Info never clears what an event already set.
    Info().then((i) => {
      if (i.hotkeyWarning) setWarning(i.hotkeyWarning);
      if (i.indexError) setIndexError(i.indexError);
    });
    IndexStatus().then((s) => s && setStatus((cur) => cur || s));
    const offs = [
      EventsOn("hotkey:warning", (w: string) => setWarning(w)),
      EventsOn("engine:error", (m: string) => setIndexError(m)),
      EventsOn("engine:status", (s: string) => setStatus(s)),
      // Each time the panel opens: back in the search box, old text selected, so typing
      // replaces it.
      EventsOn("panel:shown", () => {
        input.current?.focus();
        input.current?.select();
      }),
    ];
    return () => offs.forEach((off) => off());
  }, []);

  // Keys on the window, so they work wherever the focus is in the page.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      // During IME composition (Japanese, Chinese, Hindi input...) keys belong to it.
      if (e.isComposing || e.keyCode === 229) return;
      const i = ordered.findIndex((r) => r.id === current?.id);
      switch (e.key) {
        case "Escape":
          e.preventDefault();
          HidePanel();
          break;
        case "ArrowDown":
        case "ArrowUp": {
          if (ordered.length === 0) return;
          e.preventDefault();
          const next = e.key === "ArrowDown" ? Math.min(ordered.length - 1, i + 1) : Math.max(0, i - 1);
          setSelected(ordered[next].id);
          break;
        }
        case "Enter":
          e.preventDefault();
          if (e.ctrlKey) reveal(current);
          else open(current);
          break;
        case "c":
        case "C": {
          // Ctrl+C copies the file, unless text is selected in the search box (then it
          // copies that text, as usual).
          const el = input.current;
          const textSelected = el && el.selectionStart !== el.selectionEnd && document.activeElement === el;
          if (e.ctrlKey && !textSelected && current) {
            e.preventDefault();
            copy(current);
          }
          break;
        }
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  });

  // Keep the selected row visible while moving with the keyboard.
  useEffect(() => {
    document.getElementById(`r${current?.id}`)?.scrollIntoView({ block: "nearest" });
  }, [current?.id]);

  const expanded = query.trim() !== "";
  const footerLeft =
    notice ||
    (busy === "words" ? "Matching words…" : busy === "meaning" ? "Matching meaning…" : "") ||
    (expanded ? `${results.length} ${results.length === 1 ? "result" : "results"}` : status);

  return (
    <div className={expanded ? "panel expanded" : "panel"}>
      <div className="search">
        <svg className="search-icon" viewBox="0 0 24 24" aria-hidden="true">
          <circle cx="10.5" cy="10.5" r="6.5" />
          <line x1="15.5" y1="15.5" x2="21" y2="21" />
        </svg>
        <input
          ref={input}
          value={query}
          onChange={(e) => {
            setQuery(e.target.value);
            runSearch(e.target.value);
          }}
          placeholder="Describe it: “rent agreement”, “invoice from March”…"
          spellCheck={false}
          autoFocus
          aria-label="Search"
        />
      </div>

      {expanded && (
        <>
          <div className="body">
            <div className="list" role="listbox" aria-label="Results">
              {indexError && <p className="warning">{indexError}</p>}
              {warning && <p className="warning">{warning}</p>}
              {meaningOff && <p className="note">Searching by words only: {meaningOff}</p>}
              {results.length === 0 && busy === "" && <p className="empty">No matching files.</p>}
              {groups.map((g) => (
                <section key={g.label}>
                  <h2>{g.label}</h2>
                  {g.items.map((r) => (
                    <div
                      key={r.id}
                      id={`r${r.id}`}
                      role="option"
                      aria-selected={r.id === current?.id}
                      className={r.id === current?.id ? "row selected" : "row"}
                      onClick={() => setSelected(r.id)}
                      onDoubleClick={() => open(r)}
                    >
                      <FileIcon ext={r.ext} kind={r.kind} />
                      <div className="row-text">
                        <div className="row-title">
                          <span className="name">{r.name}</span>
                          {r.page > 0 && <span className="badge">p. {r.page}</span>}
                        </div>
                        <div className="snippet">
                          <Highlight text={r.snippet || r.folder} words={words} />
                        </div>
                      </div>
                    </div>
                  ))}
                </section>
              ))}
            </div>

            <div className="preview">
              {current && (
                <>
                  <div className="preview-media">
                    {preview?.image ? (
                      <>
                        <img src={preview.image} alt="" />
                        {preview.page > 0 && <span className="badge on-image">p. {preview.page}</span>}
                      </>
                    ) : preview?.text ? (
                      <div className="preview-text">
                        <Highlight text={preview.text} words={words} />
                      </div>
                    ) : (
                      <div className="preview-placeholder">
                        <FileIcon ext={current.ext} kind={current.kind} large />
                        {preview?.err && <p>{preview.err}</p>}
                      </div>
                    )}
                  </div>
                  <h3 className="preview-name">{current.name}</h3>
                  <p className="muted">{current.folder}</p>
                  <p className="muted">{describe(current, preview?.pages ?? 0)}</p>
                  {current.keyword && current.snippet && (
                    <div className="why">
                      <span className="why-icon">Aa</span>
                      <div>
                        <strong>Has your words</strong>
                        <p className="muted">
                          <Highlight text={current.snippet} words={words} />
                        </p>
                      </div>
                    </div>
                  )}
                  {current.meaning && (
                    <div className="why">
                      <span className="why-icon">✦</span>
                      <div>
                        <strong>Matches meaning</strong>
                        <p className="muted">Found by what it is about, not only its words.</p>
                      </div>
                    </div>
                  )}
                  <div className="actions">
                    <button className="primary" onClick={() => open(current)}>
                      Open
                    </button>
                    <button onClick={() => reveal(current)}>Show in Explorer</button>
                  </div>
                </>
              )}
            </div>
          </div>
          <footer>
            <span className="status">{footerLeft}</span>
            <span className="keys">
              <kbd>Enter</kbd> Open <kbd>Ctrl+Enter</kbd> Show in Explorer <kbd>Ctrl+C</kbd> Copy <kbd>Esc</kbd> Close
            </span>
          </footer>
        </>
      )}
    </div>
  );
}
