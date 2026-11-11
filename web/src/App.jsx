import { useEffect, useState } from "react";

const when = new Intl.DateTimeFormat("en-IN", {
  timeZone: "Asia/Kolkata",
  weekday: "short",
  day: "numeric",
  month: "short",
  hour: "2-digit",
  minute: "2-digit",
  hour12: false,
});

const expiry = (iso) => `${when.format(new Date(iso))} IST`;

function describe(b) {
  switch (b.state) {
    case "active":
      return { badge: "Active", text: `Logged in as ${b.user_id}. Valid until ${expiry(b.expires_at)}.` };
    case "expired":
      return { badge: "Expired", text: `The token for ${b.user_id} expired on ${expiry(b.expires_at)}.` };
    case "not_configured":
      return { badge: "Not configured", text: "Add this broker's API key and secret to .env, then restart serene auth." };
    case "error":
      return { badge: "Needs attention", text: "The stored login couldn't be read." };
    default:
      return { badge: "Logged out", text: "Log in to get today's access token." };
  }
}

async function post(path, payload) {
  const res = await fetch(path, {
    method: "POST",
    ...(payload && { headers: { "Content-Type": "application/json" }, body: JSON.stringify(payload) }),
  });
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(body.error || `Request failed (${res.status})`);
  return body;
}

export function LoginPanel({ broker, url, onSubmit, onCancel, busy }) {
  const [value, setValue] = useState("");
  const [copied, setCopied] = useState(false);
  const param = broker.token_param;
  const copy = async () => {
    await navigator.clipboard.writeText(url);
    setCopied(true);
  };
  const submit = (e) => {
    e.preventDefault();
    onSubmit(broker, value);
  };
  return (
    <div className="login">
      <p className="step">1. Open this URL and log in to {broker.label}.</p>
      <div className="url">
        <a href={url} target="_blank" rel="noreferrer">{url}</a>
        <button type="button" className="quiet" onClick={copy}>{copied ? "Copied" : "Copy"}</button>
      </div>
      <form onSubmit={submit}>
        <label htmlFor={`token-${broker.name}`} className="step">
          2. Paste the <code>{param}</code> from the redirected URL, or the whole URL.
        </label>
        <input
          id={`token-${broker.name}`}
          value={value}
          onChange={(e) => setValue(e.target.value)}
          autoComplete="off"
          autoCorrect="off"
          spellCheck="false"
        />
        <div className="actions">
          <button type="submit" disabled={busy || !value.trim()}>Submit</button>
          <button type="button" className="quiet" disabled={busy} onClick={() => onCancel(broker)}>Cancel</button>
        </div>
      </form>
    </div>
  );
}

export function BrokerCard({ broker, url, onLogin, onLogout, onSubmit, onCancel, busy }) {
  const { badge, text } = describe(broker);
  const canLogin = broker.state !== "not_configured";
  const showPanel = url && broker.state !== "active";
  return (
    <section className="card">
      <header>
        <h2>{broker.label}</h2>
        <span className={`badge ${broker.state}`}>{badge}</span>
      </header>
      <p>{text}</p>
      {broker.error && <p role="alert" className="error">{broker.error}</p>}
      {showPanel ? (
        <LoginPanel broker={broker} url={url} onSubmit={onSubmit} onCancel={onCancel} busy={busy} />
      ) : (
        <div className="actions">
          {broker.state === "active" ? (
            <button className="quiet" disabled={busy} onClick={() => onLogout(broker)}>Log out</button>
          ) : (
            canLogin && <button disabled={busy} onClick={() => onLogin(broker)}>Log in to {broker.label}</button>
          )}
        </div>
      )}
    </section>
  );
}

export default function App() {
  const [brokers, setBrokers] = useState(null);
  const [problem, setProblem] = useState("");
  const [busy, setBusy] = useState(false);
  const [urls, setUrls] = useState({});

  useEffect(() => {
    // The result is only in the URL for the redirect back; the API has the facts.
    if (window.location.search) window.history.replaceState(null, "", "/");
    const refresh = () =>
      fetch("/api/brokers")
        .then((r) => (r.ok ? r.json() : Promise.reject(new Error(`Request failed (${r.status})`))))
        .then(setBrokers)
        .catch((e) => setProblem(`Can't reach serene auth: ${e.message}. Run it again from the terminal.`));
    refresh();
    // Login happens in another tab, which may finish it through the callback.
    window.addEventListener("focus", refresh);
    return () => window.removeEventListener("focus", refresh);
  }, []);

  const setUrl = (b, url) => setUrls((u) => ({ ...u, [b.name]: url }));

  const login = async (b) => {
    setBusy(true);
    setProblem("");
    try {
      const { url } = await post(`/api/brokers/${b.name}/login`);
      setUrl(b, url);
    } catch (e) {
      setProblem(e.message);
    }
    setBusy(false);
  };

  const submit = async (b, value) => {
    setBusy(true);
    setProblem("");
    try {
      setBrokers(await post(`/api/brokers/${b.name}/token`, { value }));
      setUrl(b, null);
    } catch (e) {
      setProblem(e.message);
    }
    setBusy(false);
  };

  const logout = async (b) => {
    if (!window.confirm(`Log out of ${b.label}? This ends the token, including for anything using it to trade.`)) return;
    setBusy(true);
    setProblem("");
    try {
      setBrokers(await post(`/api/brokers/${b.name}/logout`));
    } catch (e) {
      setProblem(e.message);
    }
    setBusy(false);
  };

  return (
    <main>
      <h1>Broker logins</h1>
      <p className="sub">Tokens expire every morning, so log in once a day before trading or fetching data.</p>
      {problem && <p role="alert" className="error">{problem}</p>}
      {!brokers && !problem && <p className="sub">Loading…</p>}
      {brokers?.map((b) => (
        <BrokerCard
          key={b.name}
          broker={b}
          url={urls[b.name]}
          onLogin={login}
          onLogout={logout}
          onSubmit={submit}
          onCancel={(x) => setUrl(x, null)}
          busy={busy}
        />
      ))}
    </main>
  );
}
