// Highlight shows text with the query's words in bold (whole or part words, any case).
// Text is rendered as text only: never as HTML, so file content can't inject markup.
export function Highlight({ text, words }: { text: string; words: string[] }) {
  const parts = split(text, words);
  return (
    <>
      {parts.map((p, i) => (p.hit ? <b key={i}>{p.text}</b> : <span key={i}>{p.text}</span>))}
    </>
  );
}

function split(text: string, words: string[]): { text: string; hit: boolean }[] {
  const terms = words
    .flatMap((w) => w.split(/\s+/))
    .filter((w) => w.length > 0)
    .map((w) => w.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"));
  if (terms.length === 0 || text === "") return [{ text, hit: false }];
  // Longest first, so "report" wins over "rep".
  terms.sort((a, b) => b.length - a.length);
  const re = new RegExp(`(${terms.join("|")})`, "giu");
  const out: { text: string; hit: boolean }[] = [];
  let last = 0;
  for (const m of text.matchAll(re)) {
    const at = m.index ?? 0;
    if (at > last) out.push({ text: text.slice(last, at), hit: false });
    out.push({ text: m[0], hit: true });
    last = at + m[0].length;
  }
  if (last < text.length) out.push({ text: text.slice(last), hit: false });
  return out;
}
