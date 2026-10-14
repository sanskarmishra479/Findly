// Development only: lets the panel run in a normal browser (`npm run dev`) with sample
// data instead of Findly's Go backend, to check layout and behaviour quickly. main.tsx
// loads it only when import.meta.env.DEV is true, so production builds never include it.

type Listener = (...data: unknown[]) => void;
const listeners = new Map<string, Listener[]>();

const samples = [
  { id: 1, itemId: 11, name: "Renters insurance - policy summary.pdf", folder: "~\\Documents\\Home", kind: "pdf", ext: ".pdf", size: 59_000, modified: Date.parse("2025-08-25"), page: 1, snippet: "…water damage from plumbing, wherever they are in the world.", text: "Your belongings are covered against fire, smoke, theft, vandalism, windstorm and water damage from plumbing, wherever they are in the world. Electronics are covered at replacement cost." },
  { id: 2, itemId: 21, name: "Lease agreement 2024.pdf", folder: "~\\Documents\\Home", kind: "pdf", ext: ".pdf", size: 412_000, modified: Date.parse("2024-03-02"), page: 4, snippet: "Rent is due on the first day of each month.", text: "Rent is due on the first day of each month. Late payment adds a fee of 5%." },
  { id: 3, itemId: 31, name: "Meeting notes - budget.docx", folder: "~\\Documents\\Work", kind: "doc", ext: ".docx", size: 24_000, modified: Date.parse("2025-09-12"), page: 0, snippet: "We talked about the quarterly budget and travel costs.", text: "We talked about the quarterly budget and travel costs. Action: send the revised numbers by Friday." },
  { id: 4, itemId: 41, name: "Beach trip.jpg", folder: "~\\Pictures", kind: "image", ext: ".jpg", size: 3_100_000, modified: Date.parse("2025-07-01"), page: 0, snippet: "", text: "" },
];

function search(_seq: number, query: string, meaning: boolean) {
  const words = query.toLowerCase().split(/\s+/).filter(Boolean);
  const hits = samples.filter((s) => meaning || words.some((w) => (s.name + " " + s.text).toLowerCase().includes(w)));
  return hits.map((s, i) => ({ ...s, keyword: !meaning || i % 2 === 0, meaning, exact: false, score: 1 - i / 10 }));
}

function page(title: string, body: string) {
  // A drawn "page" with a highlighted line, standing in for a rendered PDF page.
  const svg = `<svg xmlns="http://www.w3.org/2000/svg" width="760" height="471"><rect width="760" height="471" fill="#fff"/><text x="40" y="60" font-family="Segoe UI" font-size="26" font-weight="700">${title}</text><rect x="38" y="105" width="300" height="26" fill="#ffd60a" opacity=".45"/><text x="40" y="124" font-family="Segoe UI" font-size="18">${body}</text>${[160, 200, 240, 280, 320].map((y) => `<rect x="40" y="${y}" width="${560 - (y % 7) * 20}" height="10" fill="#e3e3e8"/>`).join("")}</svg>`;
  return "data:image/svg+xml;base64," + btoa(svg);
}

const App: Record<string, (...a: never[]) => unknown> = {
  Info: () => ({ version: "dev", dataDir: "", hotkeyWarning: "", indexError: "", modelNote: "" }),
  IndexStatus: () => "ready. Ctrl+Shift+Space to search",
  Search: (seq: number, q: string, meaning: boolean) =>
    new Promise((r) => setTimeout(() => r({ seq, results: search(seq, q, meaning), words: q.split(/\s+/).filter(Boolean), meaningOff: "", err: "" }), meaning ? 250 : 20)),
  Preview: (seq: number, id: number) => {
    const s = samples.find((x) => x.id === id);
    if (!s) return { seq };
    if (s.kind === "pdf") return { seq, image: page("What's covered", s.snippet.slice(1, 40)), page: s.page || 1, pages: 2, text: "", err: "" };
    if (s.kind === "image") return { seq, image: "", page: 0, pages: 0, text: "", err: "No preview in the browser mock." };
    return { seq, image: "", page: 0, pages: 0, text: s.text, err: "" };
  },
  SetExpanded: () => undefined,
  HidePanel: () => undefined,
  OpenResult: () => "Open is not available in the browser mock.",
  ShowResultInExplorer: () => "Show in Explorer is not available in the browser mock.",
  CopyResult: () => "",
};

const w = window as unknown as Record<string, unknown>;
w.go = { main: { App: Object.fromEntries(Object.entries(App).map(([k, f]) => [k, (...a: never[]) => Promise.resolve(f(...a))])) } };
w.runtime = {
  EventsOnMultiple: (name: string, cb: Listener) => {
    listeners.set(name, [...(listeners.get(name) ?? []), cb]);
    return () => listeners.set(name, (listeners.get(name) ?? []).filter((x) => x !== cb));
  },
  EventsOff: () => undefined,
  EventsEmit: (name: string, ...data: unknown[]) => (listeners.get(name) ?? []).forEach((cb) => cb(...data)),
};
export {};
