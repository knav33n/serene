// Manual Nifty trades (serene manual, internal/manual): a strike bought with a LIMIT order at the price
// typed (at market with none), its target a resting LIMIT sell, on the brokers ticked; the brokers'
// open F&O positions, each squared off by its Exit; and today's trades as each broker has them,
// a manual one with its target and Sell now.
import { useState } from "react";
import { Button, api, useSay } from "./ui.jsx";

export const BROKERS = { zerodha: "Zerodha", upstox: "Upstox", fyers: "Fyers", icici: "ICICI" };
export const rupees = v => v == null ? "–" : (v < 0 ? "−₹" : "₹") + Math.abs(Math.round(v)).toLocaleString("en-IN");
const px = v => v == null ? "–" : Number(v).toFixed(2);
const sign = v => v == null || v === 0 ? "" : v > 0 ? "up" : "down";
const Money = ({ v, strong }) => <span className={"money " + sign(v)}>{strong ? <b>{rupees(v)}</b> : rupees(v)}</span>;
const Tag = ({ broker }) => <span className={"btag " + broker}>{BROKERS[broker]}</span>;

export default function Manual({ man, asking, load }) {
  const say = useSay();
  const [form, setForm] = useState({ expiry: "", strike: "", side: "CE", limit: "", target: "2", capital: "700000" });
  const [brokers, setBrokers] = useState({ zerodha: true, upstox: true, fyers: false, icici: false });
  const set = (k, v) => setForm(f => ({ ...f, [k]: v }));
  const picked = Object.keys(BROKERS).filter(b => brokers[b]);
  // The nearest expiry unless another was picked
  const expiry = man && man.expiries.includes(form.expiry) ? form.expiry : man?.expiries[0] || "";

  async function buy() {
    const strike = parseFloat(form.strike), limit = form.limit.trim() === "" ? null : parseFloat(form.limit);
    const target_pct = parseFloat(form.target), capital = parseFloat(form.capital);
    if (!expiry) return say("no expiry: Refresh", false);
    if (!(strike > 0)) return say("enter the strike", false);
    if (limit !== null && !(limit > 0)) return say("the limit price must be above 0, or empty for market", false);
    if (!(target_pct > 0) || !(capital > 0)) return say("the target and the capital must be above 0", false);
    if (!picked.length) return say("tick a broker", false);
    const what = `NIFTY ${expiry} ${strike} ${form.side} ${limit === null ? "at market" : `with a limit at ${px(limit)}`}, `
      + `target ${target_pct}% net (a LIMIT sell), capital ${rupees(capital)}`;
    const on = picked.map(b => BROKERS[b]).join(" and ");
    const itm = form.side === "CE" ? strike - 100 : strike + 100; // ITMDepth in internal/manual
    const split = `\n\nAn account with over ${rupees(500000)} free splits it: half to ${strike} ${form.side} (ATM), half to ${itm} ${form.side} (ITM).`;
    if (!confirm(`REAL order on ${on}: buy ${what}?${split}`)) return;
    const r = await api("api/manual/buy", { expiry, strike, option_type: form.side, limit, target_pct, capital, brokers: picked });
    say([...r.trades.map(t => `bought ${t.qty} ${t.contract} on ${BROKERS[t.broker]} @ ${px(t.entry)} · target ${px(t.target)}`),
         ...(r.warnings || []), ...(r.errors || []).map(e => "not bought on " + e)],
        !r.errors?.length && !r.warnings?.length);
    await load();
  }

  async function exit(p, what = "Square off") {
    if (!confirm(`${what} ${p.qty} ${p.name} on ${BROKERS[p.broker]} at market now?`)) return;
    const r = await api("api/manual/exit", { broker: p.broker, symbol: p.symbol, exchange: p.exchange, product: p.product });
    say(`squared off ${r.qty} ${p.name} on ${BROKERS[p.broker]} @ ${px(r.price)}`);
    await load();
  }
  const positions = Object.keys(BROKERS).flatMap(b => (man?.positions?.[b]?.rows || []).map(p => ({ ...p, broker: b })));
  const unread = Object.keys(BROKERS).filter(b => man?.positions?.[b]?.error);
  const day = man?.trades || {};
  const strike = form.strike.trim();

  return <>
    <section className={"ticket " + form.side.toLowerCase()}>
      <div className="sides" role="radiogroup" aria-label="Side">
        {[["CE", "Call"], ["PE", "Put"]].map(([s, word]) => <label key={s} className={"sidebtn " + s.toLowerCase()}>
          <input type="radio" name="man-side" value={s} checked={form.side === s} onChange={() => set("side", s)} />
          <b>{s}</b><span>{word}</span></label>)}
      </div>
      <div className="fields">
        <label className="f wide"><span>Expiry</span><select value={expiry} onChange={e => set("expiry", e.target.value)}>
          {!man ? <option value="">{asking ? "loading…" : "couldn't load"}</option>
            : man.expiries.map((e, i) => <option key={e} value={e}>{i === 0 ? `${e} · nearest` : e}</option>)}
        </select></label>
        <label className="f wide big"><span>Strike</span><input type="text" inputMode="numeric" placeholder="22750" value={form.strike}
          onChange={e => set("strike", e.target.value)} /></label>
        <label className="f"><span>Limit ₹</span><input type="text" inputMode="decimal" placeholder="market" value={form.limit}
          onChange={e => set("limit", e.target.value)} /></label>
        <label className="f"><span>Target %</span><input type="text" inputMode="decimal" value={form.target}
          onChange={e => set("target", e.target.value)} /></label>
        <label className="f"><span>Capital ₹</span><input type="text" inputMode="numeric" value={form.capital}
          onChange={e => set("capital", e.target.value)} /></label>
      </div>
      <div className="go">
        <div className="pick">
          {Object.entries(BROKERS).map(([b, name]) => { const f = man?.free?.[b];
            return <label className={"bpick" + (brokers[b] ? " on" : "")} key={b} title={f?.error || ""}>
              <input type="checkbox" checked={brokers[b]} onChange={e => setBrokers({ ...brokers, [b]: e.target.checked })} />
              <span className="name">{name}</span>
              <span className="free">{f ? (f.free != null ? `${rupees(f.free)} free` : "margin ?") : ""}</span>
            </label>; })}
        </div>
        <Button className={"b buy " + form.side.toLowerCase()} onClick={buy} aria-label="Buy">
          Buy <span className="what">{strike ? `${strike} ${form.side}` : form.side}</span></Button>
      </div>
    </section>

    <section className="block">
      <div className="bhead"><h2>Positions</h2><span className="count">{positions.length}</span>
        {unread.map(b => <span key={b} className="warn" title={man.positions[b].error}>{BROKERS[b]} unreadable</span>)}</div>
      <div className="sheet"><table>
        <thead><tr><th>Broker</th><th>Contract</th><th>Product</th><th className="n">Qty</th><th className="n">Avg</th>
          <th className="n">LTP</th><th className="n">P&amp;L</th><th></th></tr></thead>
        <tbody>{!positions.length ? <tr><td colSpan={8} className="empty">Flat</td></tr> : positions.map(p =>
          <tr key={`${p.broker} ${p.symbol} ${p.product}`}>
            <td><Tag broker={p.broker} /></td><td className="mono">{p.name}</td><td className="dim">{p.product}</td>
            <td className="n">{p.qty}</td><td className="n">{px(p.avg)}</td><td className="n">{px(p.ltp)}</td>
            <td className="n"><Money v={p.pnl} strong /></td>
            <td className="n"><Button className="b danger" onClick={() => exit(p)}>Exit</Button></td>
          </tr>)}</tbody>
      </table></div>
    </section>

    <section className="block">
      <div className="bhead"><h2>Today</h2><span className="count">{day.rows?.length || 0}</span>
        {Object.entries(day.errors || {}).map(([b, e]) => <span key={b} className="warn" title={e}>{BROKERS[b]} unreadable</span>)}</div>
      {Object.keys(day.totals || {}).length > 0 && <div className="tiles">
        {Object.entries(day.totals).map(([b, t]) => <div className={"tile " + b} key={b}
          title={[t.priced < t.orders ? `charges for ${t.priced} of ${t.orders} orders` : "", t.note || ""].filter(Boolean).join(" · ")}>
          <Tag broker={b} />
          <div className="v"><Money v={t.net} /></div>
          <div className="k">P&amp;L {rupees(t.pnl)} · charges {rupees(t.charges)}</div>
        </div>)}
      </div>}
      <div className="sheet"><table>
        <thead><tr><th>Broker</th><th>Contract</th><th className="n">Bought</th><th className="n">Sold</th>
          <th className="n">Open</th><th className="n">LTP</th><th className="n">P&amp;L</th><th className="n">Charges</th>
          <th className="n">Net</th><th>By</th><th></th></tr></thead>
        <tbody>{!day.rows?.length ? <tr><td colSpan={11} className="empty">No trades yet</td></tr> : day.rows.map(r =>
          <tr key={`${r.broker} ${r.symbol} ${r.product}`}>
            <td><Tag broker={r.broker} /></td>
            <td><span className="mono">{r.name}</span> <span className="dim">{r.product}</span>
              <div className="orders">
                {r.carried ? <span>{r.carried} carried</span> : null}
                {r.orders.map(o => <span key={o.order_id} className={o.side === "BUY" ? "ob" : "os"}>
                  {o.time} {o.side} {o.qty} @ {px(o.price)}</span>)}
              </div></td>
            <td className="n">{r.bought ? `${r.bought} @ ${px(r.buy_avg)}` : "–"}</td>
            <td className="n">{r.sold ? `${r.sold} @ ${px(r.sell_avg)}` : "–"}</td>
            <td className="n">{r.qty || "–"}</td><td className="n">{px(r.ltp)}</td>
            <td className="n"><Money v={r.pnl} /></td><td className="n dim">{rupees(r.charges)}</td>
            <td className="n"><Money v={r.net} strong /></td>
            <td>{r.by && <span className="by">{r.by}</span>}{r.target != null && <div className="dim small">target {px(r.target)}</div>}</td>
            <td className="n">{r.manual && r.qty !== 0 &&
              <Button className="b danger" onClick={() => exit({ ...r, broker: r.broker }, "Sell")}>Sell now</Button>}</td>
          </tr>)}</tbody>
      </table></div>
    </section>
  </>;
}
