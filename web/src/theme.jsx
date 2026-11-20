// The light / dark switch every page has. Until it's used a page follows the system; the choice
// is kept in this browser, for this page's address. Importing this applies it, before the page draws.
import { useState } from "react";

const KEY = "serene-theme";
const root = document.documentElement;

function stored() {
  try { return localStorage.getItem(KEY); } catch { return null; }
}

const saved = stored();
if (saved === "light" || saved === "dark") root.dataset.theme = saved;

const current = () => root.dataset.theme || (window.matchMedia?.("(prefers-color-scheme: dark)").matches ? "dark" : "light");

export function ThemeSwitch() {
  const [theme, setTheme] = useState(current);
  const dark = theme === "dark";
  function flip() {
    const next = dark ? "light" : "dark";
    root.dataset.theme = next;
    try { localStorage.setItem(KEY, next); } catch { /* it just won't be remembered */ }
    setTheme(next);
  }
  return <button type="button" className="themeswitch" role="switch" aria-checked={dark} aria-label="Dark theme" onClick={flip}>
    <span className="track"><span className="knob" /></span>{dark ? "Dark" : "Light"}
  </button>;
}
