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
