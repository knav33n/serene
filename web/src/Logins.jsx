// serene auth's page: each broker's login today, a Log in that opens the broker's page in a new
// tab and finishes by itself when the broker redirects here (or from the code pasted back), and
// where each login was handed on (project_calm here, hearth). Tokens never reach this page.
import { useCallback, useEffect, useRef, useState } from "react";
import { Button, Message, Say, api } from "./ui.jsx";
import { ThemeSwitch } from "./theme.jsx";

const IST = "Asia/Kolkata";
const SENT_TO = { calm: "project_calm here", hearth: "hearth" };

const clock = d => d.toLocaleTimeString("en-GB", { timeZone: IST, hour: "2-digit", minute: "2-digit" });
const dayOf = d => d.toLocaleDateString("en-GB", { timeZone: IST, weekday: "long", day: "numeric", month: "long" });
const sameDay = (a, b) => dayOf(a) === dayOf(b);

function left(ms) {
  const m = Math.max(0, Math.round(ms / 60000));
  return m >= 60 ? `${Math.floor(m / 60)}h ${String(m % 60).padStart(2, "0")}m` : `${m}m`;
}

function until(at, now) {
  const when = sameDay(at, now) ? clock(at) : `${clock(at)} ${at.toLocaleDateString("en-GB", { timeZone: IST, weekday: "short" })}`;
  return `until ${when}`;
}

function names(list) {
  return list.length < 2 ? list.join("") : `${list.slice(0, -1).join(", ")} and ${list.at(-1)}`;
}

// What's left to do today, in one sentence
function verdict(brokers) {
  const ready = brokers.filter(b => b.state !== "not_configured");
  const todo = ready.filter(b => b.state !== "active").map(b => b.label);
  if (!ready.length) return "Add a broker's API key and secret to .env to log in.";
  if (!todo.length) return "Every broker is logged in for today.";
  if (todo.length === ready.length) return "No broker is logged in yet today.";
  return `${names(todo)} still ${todo.length === 1 ? "needs" : "need"} today's login.`;
}

function Life({ b, now }) {
  if (b.state !== "active" || !b.expires_at) return null;
  const end = new Date(b.expires_at), start = b.issued_at ? new Date(b.issued_at) : null;
  const used = start ? Math.min(1, Math.max(0, (now - start) / (end - start))) : 0;
  return <div className="life" role="meter" aria-label="Login used up" aria-valuemin={0} aria-valuemax={100}
    aria-valuenow={Math.round(used * 100)}>
    <span style={{ width: `${(1 - used) * 100}%` }} />
  </div>;
}

function State({ b, now }) {
  switch (b.state) {
    case "active": {
      const end = new Date(b.expires_at);
      return <p className="state on">Logged in as <b className="mono">{b.user_id}</b>, {left(end - now)} left, {until(end, now)}</p>;
    }
    case "expired":
      return <p className="state">Yesterday's login{b.user_id && <> as <span className="mono">{b.user_id}</span></>} has expired</p>;
    case "not_configured":
      return <p className="state">Not set up: add its API key and secret to .env, then restart serene auth</p>;
    case "error":
      return <p className="state bad">The stored login can't be read</p>;
    default:
      return <p className="state">Not logged in</p>;
  }
}

function Sent({ list }) {
  if (!list?.length) return null;
  return <ul className="sent">{list.map(d => <li key={d.to} className={d.state} title={d.error || ""}>
    {d.state === "sending" ? <span className="spin" /> : <i aria-hidden="true" />}
    {d.state === "ok" ? `Sent to ${SENT_TO[d.to] || d.to}` : d.state === "sending" ? `Sending to ${SENT_TO[d.to] || d.to}`
      : `Not sent to ${SENT_TO[d.to] || d.to}: ${d.error}`}
  </li>)}</ul>;
}

function Broker({ b, now, set, say }) {
  const [value, setValue] = useState("");
  const input = useRef(null);
  const open = b.pending;
  useEffect(() => { if (open) input.current?.focus(); }, [open]);

  async function login() {
    // Opened before the request so the browser treats it as the click's, then pointed at the broker
    const tab = window.open("", "_blank");
    try {
      const { url } = await api(`api/brokers/${b.name}/login`);
      if (tab) { tab.opener = null; tab.location.href = url; } else window.location.assign(url);
      set(await api("api/brokers"));
    } catch (e) {
      tab?.close();
      throw e;
    }
  }
  async function finish() {
    set(await api(`api/brokers/${b.name}/token`, { value }));
    setValue("");
    say(`Logged in to ${b.label}.`);
  }
  const cancel = async () => set(await api(`api/brokers/${b.name}/cancel`));
  async function logout() {
    if (!confirm(`Log out of ${b.label}? Its token is revoked at the broker, and anything using it stops.`)) return;
    set(await api(`api/brokers/${b.name}/logout`));
    say(`Logged out of ${b.label}.`);
  }
  const resend = async () => set(await api(`api/brokers/${b.name}/send`));
  const failed = b.sent?.some(d => d.state === "failed");
  const canLogin = b.state !== "not_configured" && !b.wait;

  return <li className={`broker ${b.state}${open ? " open" : ""}`}>
    <div className="who">
      <h2>{b.label}</h2>
      <State b={b} now={now} />
      <Life b={b} now={now} />
    </div>
    <div className="acts">
      {b.state === "active" && failed && <Button onClick={resend}>Send again</Button>}
      {b.state === "active" && <Button className="b quiet" onClick={logout}>Log out</Button>}
      {canLogin && !open && <Button className={b.state === "active" ? "b" : "b primary"} onClick={login}>
        {b.state === "active" ? "Log in again" : "Log in"}</Button>}
    </div>
    {b.wait && <p className="hint">{b.wait}</p>}
    {b.error && <p className="hint bad">{b.error}</p>}
    <Sent list={b.sent} />
    {open && <form className="paste" onSubmit={e => e.preventDefault()}>
      <p>Log in on the {b.label} tab. It finishes here by itself when {b.label} sends you back; if it lands
        somewhere else, paste the <span className="mono">{b.token_param}</span> or the whole address.</p>
      <div className="row">
        <input ref={input} type="password" autoComplete="off" spellCheck={false} value={value}
          placeholder={`${b.token_param}=… or the address you landed on`} aria-label={`${b.label} ${b.token_param}`}
          onChange={e => setValue(e.target.value)} onKeyDown={e => { if (e.key === "Enter" && value.trim()) finish().catch(err => say(err.message, false)); }} />
        <Button className="b primary" onClick={finish} disabled={!value.trim()}>Finish login</Button>
        <Button className="b quiet" onClick={cancel}>Cancel</Button>
      </div>
    </form>}
  </li>;
}

export default function Logins() {
  const [msg, setMsg] = useState(null);
  const say = useCallback((text, ok = true) => setMsg({ text, ok }), []);
  const [brokers, setBrokers] = useState(null);
  const [now, setNow] = useState(() => new Date());

  const load = useCallback(async () => {
    try { setBrokers(await api("api/brokers")); } catch (e) { say(e.message, false); }
  }, [say]);

  // Back from a broker's redirect: say how it went, then tidy the address
  useEffect(() => {
    const q = new URLSearchParams(window.location.search);
    const done = q.get("done"), failed = q.get("failed");
    if (done || failed) {
      api("api/brokers").then(list => {
        const b = list.find(x => x.name === (done || failed));
        const label = b?.label || done || failed;
        say(done ? `Logged in to ${label}. You can close this tab.` : `The ${label} login failed: ${b?.error || "log in again"}`, !!done);
      }).catch(() => {});
      window.history.replaceState(null, "", "/");
    }
  }, [say]);

  // Quickly while a login is open or being handed on, slowly otherwise
  const busy = brokers?.some(b => b.pending || b.sent?.some(d => d.state === "sending"));
  useEffect(() => {
    load();
    const t = setInterval(() => { setNow(new Date()); load(); }, busy ? 1500 : 30000);
    const back = () => { setNow(new Date()); load(); };
    window.addEventListener("focus", back);
    return () => { clearInterval(t); window.removeEventListener("focus", back); };
  }, [load, busy]);

  return <Say.Provider value={say}>
    <main className="logins">
      <header>
        <div className="titlebar">
          <p className="today">{dayOf(now)}, {clock(now)} IST</p>
          <ThemeSwitch />
        </div>
        <h1 aria-live="polite">{brokers ? verdict(brokers) : "Reading today's logins…"}</h1>
      </header>
      <Message msg={msg} />
      {brokers && <ul className="brokers">
        {brokers.map(b => <Broker key={b.name} b={b} now={now} set={setBrokers} say={say} />)}
      </ul>}
      <p className="note">Tokens are encrypted before they're stored, and never shown here. Zerodha's and Upstox's
        are also sent to hearth and both machines' project_calm. This page answers on 127.0.0.1 only;
        stop it with Ctrl+C in the terminal.</p>
    </main>
  </Say.Provider>;
}
