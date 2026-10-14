import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { Panel } from "./Panel";
import "./styles.css";

async function start() {
  // In a plain browser during development (`npm run dev`, no Go backend), use sample
  // data. import.meta.env.DEV is false in production builds, so Vite leaves the mock out.
  if (import.meta.env.DEV && !("go" in window)) {
    await import("./devmock");
  }
  createRoot(document.getElementById("root")!).render(
    <StrictMode>
      <Panel />
    </StrictMode>,
  );
}

start();
