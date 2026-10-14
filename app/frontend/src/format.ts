import { main } from "../wailsjs/go/models";

// groupOf names the result group a file kind belongs to (headings in the result list).
export function groupOf(kind: string): string {
  switch (kind) {
    case "pdf":
    case "doc":
    case "text":
      return "Documents";
    case "image":
      return "Images";
    case "audio":
      return "Audio";
    case "video":
      return "Videos";
    default:
      return "Other";
  }
}

const kindName: Record<string, string> = { pdf: "PDF", doc: "Document", text: "Text", image: "Image", audio: "Audio", video: "Video" };

// describe is the preview's detail line: "PDF · Aug 25, 2025 · 2 pages · 59 KB".
export function describe(r: main.SearchResult, pages: number): string {
  const parts = [kindName[r.kind] ?? (r.ext.replace(/^\./, "").toUpperCase() || "File")];
  if (r.modified > 0) {
    parts.push(new Date(r.modified).toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" }));
  }
  if (pages > 0) parts.push(pages === 1 ? "1 page" : `${pages} pages`);
  parts.push(bytes(r.size));
  return parts.join(" · ");
}

export function bytes(n: number): string {
  if (n < 1024) return `${n} B`;
  const units = ["KB", "MB", "GB"];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v < 10 ? v.toFixed(1) : Math.round(v)} ${units[i]}`;
}
