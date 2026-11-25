// Hearth's page: the Gold & Silver desk. Locally there's no login (the server listens on loopback
// only); served publicly (--public) the Gate comes first.
import { useCallback, useEffect, useState } from "react";
import Metals from "./Metals.jsx";
import Gate from "./Gate.jsx";
import { Logo, Message, Say, Spin, api } from "./ui.jsx";
import { ThemeSwitch } from "./theme.jsx";

export default function App() {
  const [msg, setMsg] = useState(null);
  const say = useCallback((text, ok = true) => setMsg({ text, ok }), []);
  // null until the server says; then whether it's gated and whether this browser is in
  const [session, setSession] = useState(null);
  const [mcx, setMcx] = useState({ data: null, asking: false });

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
  useEffect(() => { if (session?.in) loadMcx(); }, [session?.in, loadMcx]);

  async function logout() {
    try { await api("api/logout"); } catch { /* the cookie goes either way on the next 401 */ }
    setMcx({ data: null, asking: false });
    setMsg(null);
    setSession(s => ({ ...s, in: false }));
  }

  if (!session) return <main className="gate"><Spin /></main>;
  if (!session.in) return <Gate onIn={() => setSession(s => ({ ...s, in: true }))} />;

  return <Say.Provider value={say}>
    <main className="desk">
      <header className="topbar">
        <div className="brand"><Logo /><h1>Hearth</h1></div>
        <div className="tools">
          <button type="button" className="refresh" onClick={loadMcx} disabled={mcx.asking} aria-label="Refresh" title="Refresh">
            {mcx.asking ? <Spin /> : "↻"}</button>
          <ThemeSwitch />
          {session.gated && <button type="button" className="b quiet" onClick={logout}>Log out</button>}
        </div>
      </header>
      <Message msg={msg} />
      <Metals data={mcx.data} asking={mcx.asking} />
    </main>
  </Say.Provider>;
}
