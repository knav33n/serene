// serene manual's page: the Manual form, the open positions and today's trades. Locally there's no
// login (the server listens on loopback only); served publicly (--public) the Gate comes first.
import { useCallback, useEffect, useState } from "react";
import Manual, { rupees } from "./Manual.jsx";
import Metals from "./Metals.jsx";
import Gate from "./Gate.jsx";
import { Logo, Message, Say, Spin, api } from "./ui.jsx";
import { ThemeSwitch } from "./theme.jsx";

export default function App() {
  const [msg, setMsg] = useState(null);
  const say = useCallback((text, ok = true) => setMsg({ text, ok }), []);
  const [man, setMan] = useState({ data: null, asking: false });
  // null until the server says; then whether it's gated and whether this browser is in
  const [session, setSession] = useState(null);
  // #metals is the Gold & Silver page; anything else the NIFTY desk
  const [tab, setTab] = useState(() => location.hash === "#metals" ? "metals" : "nifty");
  const [mcx, setMcx] = useState({ data: null, asking: false });
  useEffect(() => {
    const on = () => setTab(location.hash === "#metals" ? "metals" : "nifty");
    addEventListener("hashchange", on);
    return () => removeEventListener("hashchange", on);
  }, []);

  const load = useCallback(async () => {
    setMan(s => ({ ...s, asking: true }));
    try {
      setMan({ data: await api("api/manual"), asking: false });
    } catch (e) {
      setMan(s => ({ ...s, asking: false }));
      if (e.status === 401) setSession(s => ({ gated: true, ...s, in: false }));
      else say(e.message, false);
    }
  }, [say]);

  useEffect(() => {
    api("api/session").then(setSession, e => { say(e.message, false); setSession({ gated: false, in: true }); });
  }, [say]);
  const loadMcx = useCallback(async () => {
    setMcx(s => ({ ...s, asking: true }));
    try {
      setMcx({ data: await api("api/mcx"), asking: false });
    } catch (e) {
      setMcx(s => ({ ...s, asking: false }));
      if (e.status === 401) setSession(s => ({ gated: true, ...s, in: false }));
      else say(e.message, false);
    }
  }, [say]);
  useEffect(() => { if (session?.in && tab === "nifty") load(); }, [session?.in, tab, load]);
  useEffect(() => { if (session?.in && tab === "metals") loadMcx(); }, [session?.in, tab, loadMcx]);

  async function logout() {
    try { await api("api/logout"); } catch { /* the cookie goes either way on the next 401 */ }
    setMan({ data: null, asking: false });
    setMsg(null);
    setSession(s => ({ ...s, in: false }));
  }

  if (!session) return <main className="gate"><Spin /></main>;
  if (!session.in) return <Gate onIn={() => setSession(s => ({ ...s, in: true }))} />;

  const metals = tab === "metals";
  const busy = metals ? mcx.asking : man.asking;
  const totals = Object.values(man.data?.trades?.totals || {});
  const net = totals.length ? totals.reduce((s, t) => s + (t.net || 0), 0) : null;

  return <Say.Provider value={say}>
    <main className="desk">
      <header className="topbar">
        <div className="brand"><Logo /><h1>Hearth</h1>
          <nav className="pagetabs" aria-label="Pages">
            <a href="#" aria-current={metals ? undefined : "page"}>NIFTY</a>
            <a href="#metals" aria-current={metals ? "page" : undefined}>Gold &amp; Silver</a>
          </nav></div>
        {!metals && <div className="daynet">
          <span className="k">Day net</span>
          <span className={"v " + (net > 0 ? "up" : net < 0 ? "down" : "")}>{net == null ? "–" : rupees(net)}</span>
        </div>}
        <div className="tools">
          <button type="button" className="refresh" onClick={metals ? loadMcx : load} disabled={busy} aria-label="Refresh" title="Refresh">
            {busy ? <Spin /> : "↻"}</button>
          <ThemeSwitch />
          {session.gated && <button type="button" className="b quiet" onClick={logout}>Log out</button>}
        </div>
      </header>
      <Message msg={msg} />
      {metals ? <Metals data={mcx.data} asking={mcx.asking} />
        : <Manual man={man.data} asking={man.asking} load={load} />}
    </main>
  </Say.Provider>;
}
