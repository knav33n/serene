// The public page's login (serene manual --public): a password and the authenticator's 6 digits.
import { useState } from "react";
import { api } from "./ui.jsx";

export default function Gate({ onIn }) {
  const [pw, setPw] = useState("");
  const [code, setCode] = useState("");
  const [err, setErr] = useState(null);
  const [busy, setBusy] = useState(false);

  async function submit(e) {
    e.preventDefault();
    if (busy) return;
    setBusy(true);
    setErr(null);
    try {
      await api("api/login", { password: pw, code });
      onIn();
    } catch (e) {
      setErr(e.message);
      setCode("");
    } finally {
      setBusy(false);
    }
  }

  return <main className="gate">
    <form className="gatecard" onSubmit={submit}>
      <div className="brand"><span className="dot" /><h1>Manual Trades</h1></div>
      <label className="f"><span>Password</span>
        <input type="password" autoComplete="current-password" autoFocus value={pw} onChange={e => setPw(e.target.value)} /></label>
      <label className="f big"><span>Code</span>
        <input type="text" inputMode="numeric" autoComplete="one-time-code" placeholder="000000" maxLength={6} value={code}
          onChange={e => setCode(e.target.value.replace(/\D/g, ""))} /></label>
      {err && <div className="msg err" role="status">{err}</div>}
      <button type="submit" className={"b unlock" + (busy ? " busy" : "")} disabled={busy || !pw || code.length !== 6}>Unlock</button>
    </form>
  </main>;
}
