import { writeFileSync } from "node:fs";
import { defineConfig, type Plugin } from "vite";
import react from "@vitejs/plugin-react";

// Content-Security-Policy for the built app (plan P-2): only files bundled into the exe
// may load, no remote scripts, styles, fonts, images or connections. Added only to the
// production build, because the dev server needs inline scripts for hot reload.
const csp = [
  "default-src 'self'",
  "script-src 'self'",
  "style-src 'self'",
  "img-src 'self' data:",
  "font-src 'self'",
  "connect-src 'self'",
  "object-src 'none'",
  "base-uri 'none'",
  "form-action 'none'",
  "frame-ancestors 'none'",
].join("; ");

function contentSecurityPolicy(): Plugin {
  return {
    name: "findly-csp",
    apply: "build",
    transformIndexHtml(html) {
      // Right after the charset declaration, which must come first.
      const charset = '<meta charset="UTF-8" />';
      if (!html.includes(charset)) throw new Error("index.html: charset meta not found");
      return html.replace(
        charset,
        `${charset}\n    <meta http-equiv="Content-Security-Policy" content="${csp}" />`,
      );
    },
  };
}

// The Go code embeds dist/ and fails to compile if it is empty, so a placeholder file is
// kept there (and in git) after every build.
function keepDistPlaceholder(): Plugin {
  return {
    name: "findly-keep-dist",
    apply: "build",
    closeBundle() {
      writeFileSync(new URL("dist/.gitkeep", import.meta.url), "");
    },
  };
}

export default defineConfig({
  plugins: [react(), contentSecurityPolicy(), keepDistPlaceholder()],
  server: {
    host: "127.0.0.1", // the dev server is reachable from this PC only
    strictPort: false,
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
    sourcemap: false,
    // WebView2 is evergreen Chromium.
    target: "chrome120",
  },
});
