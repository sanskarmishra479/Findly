// A file-type icon: a page with a folded corner and the file's extension. (Real
// thumbnails come with the images phase.)
const tint: Record<string, string> = {
  pdf: "#e5484d",
  doc: "#3b82f6",
  text: "#8b8d98",
  image: "#22a06b",
  audio: "#e5a00d",
  video: "#8e4ec6",
  other: "#8b8d98",
};

export function FileIcon({ ext, kind, large }: { ext: string; kind: string; large?: boolean }) {
  const label = ext.replace(/^\./, "").slice(0, 4).toUpperCase() || "FILE";
  const size = large ? 64 : 32;
  return (
    <svg className="file-icon" width={size} height={size * 1.2} viewBox="0 0 40 48" aria-hidden="true">
      <path d="M4 2h22l10 10v32a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2z" className="page" />
      <path d="M26 2v8a2 2 0 0 0 2 2h8" className="fold" />
      <rect x="2" y="28" width="36" height="12" rx="2" fill={tint[kind] ?? tint.other} />
      <text x="20" y="37.5" textAnchor="middle" className="ext">
        {label}
      </text>
    </svg>
  );
}
