import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import App from "./App";
import { applyTheme, getTheme } from "./lib/theme";
import "./index.css";

// Apply persisted theme at module load so React state and data-theme agree
// even if the pre-paint theme-init.js script is missing or blocked.
applyTheme(getTheme());

const root = document.getElementById("root");
if (!root) {
  throw new Error("root element not found");
}

createRoot(root).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
