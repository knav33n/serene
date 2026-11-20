// The page's API and its small pieces: the message line, buttons that show
// they're working, badges and loaders.
import { createContext, useContext, useEffect, useRef, useState } from "react";

// A POST of JSON to serene manual's API. The server answers {ok, result} or
// {ok: false, error}; a failure's error carries the HTTP status.
export async function api(path, body = {}) {
  const r = await fetch(path, { method: "POST", body: JSON.stringify(body), headers: { "Content-Type": "application/json" } });
  const j = r.headers.get("Content-Type")?.startsWith("application/json") ? await r.json() : null;
  if (!r.ok || !j) throw Object.assign(new Error(j?.error || `${r.status} ${r.statusText}`), { status: r.status });
  if (!j.ok) throw new Error(j.error);
  return j.result;
}

// The message line under the title: say(text or lines, ok)
export const Say = createContext(() => {});
export const useSay = () => useContext(Say);

export function Message({ msg }) {
  const ref = useRef(null);
  useEffect(() => { ref.current?.scrollIntoView?.({ block: "nearest" }); }, [msg]);
  if (!msg) return null;
  return <div className={"msg " + (msg.ok ? "ok" : "err")} role="status" ref={ref}>
    {Array.isArray(msg.text) ? msg.text.join("\n") : msg.text}</div>;
}

// A button that's disabled and turns while its work runs; a failure goes to the message line
export function Button({ onClick, className = "b", disabled, children, ...rest }) {
  const say = useSay();
  const [busy, setBusy] = useState(false);
  async function click(e) {
    setBusy(true);
    try { await onClick(e); } catch (err) { say(err.message, false); } finally { setBusy(false); }
  }
  return <button type="button" className={className + (busy ? " busy" : "")} disabled={disabled || busy}
    onClick={click} {...rest}>{children}</button>;
}

export const Spin = () => <span className="spin" />;
export const Wait = ({ on, children }) => <span className="wait">{on && <><Spin />{children}</>}</span>;

// Hearth's mark: a fireplace arch on its hearthstone with a flame inside.
// The arch takes the ink and the flame the marigold accent, so it follows the theme.
export function Logo({ size = 32 }) {
  return <svg className="logo" width={size} height={size} viewBox="0 0 32 32" aria-hidden="true">
    <path d="M7 27V15a9 9 0 0 1 18 0v12" fill="none" stroke="var(--ink)" strokeWidth="2.6" strokeLinecap="round" />
    <path d="M3.5 27.5h25" stroke="var(--ink)" strokeWidth="2.6" strokeLinecap="round" />
    <path d="M16 11.5c0 0 5.6 4.9 5.6 9.6a5.6 5.6 0 0 1-11.2 0c0-2.4 1.5-4.4 2.9-5.7.1 1.5.8 2.6 1.8 3.1-.3-2.8.2-5 .9-7z" fill="var(--accent)" />
    <path d="M16 19.2c0 0 2.4 1.9 2.4 3.9a2.4 2.4 0 0 1-4.8 0c0-2 2.4-3.9 2.4-3.9z" fill="color-mix(in srgb, var(--accent) 45%, var(--panel))" />
  </svg>;
}
