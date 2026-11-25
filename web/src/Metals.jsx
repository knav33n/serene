// Hearth's Gold & Silver page (POST /api/mcx): a card per metal with everything it has made, the
// live strategies' state, the open MCX positions on Zerodha and the strategies' closed trades.
import { useState } from "react";
import { rupees } from "./Manual.jsx";

const METALS = { gold: "Gold", silver: "Silver" };
const px = v => v == null ? "–" : Number(v).toLocaleString("en-IN", { maximumFractionDigits: 2 });
const tone = v => v == null || v === 0 ? "" : v > 0 ? "up" : "down";
const Money = ({ v, strong }) => <span className={"money " + tone(v)}>{strong ? <b>{rupees(v)}</b> : rupees(v)}</span>;
const Side = ({ s }) => <span className={"mside " + s}>{s}</span>;
const Metal = ({ m }) => <span className={"mtag " + m}>{METALS[m] || m}</span>;

export default function Metals({ data, asking }) {
  const [only, setOnly] = useState("all");
  const show = r => only === "all" || r.metal === only;
  if (!data) return <section className="block"><div className="sheet"><table><tbody>
    <tr><td className="empty">{asking ? "Loading…" : "Couldn't load"}</td></tr></tbody></table></div></section>;
  const positions = (data.positions || []).filter(show), trades = (data.trades || []).filter(show);

  return <>
    <section className="mcards">
      {Object.entries(METALS).map(([k, name]) => { const m = data.metals?.[k] || {};
        return <div className={"mcard " + k} key={k}>
          <div className="mhead"><h2>{name}</h2>
            <span className="dim">{m.lots ? `${m.lots > 0 ? "long" : "short"} ${Math.abs(m.lots)} lot open` : "flat"}</span></div>
          <div className="mnet"><Money v={m.net} /></div>
          <div className="msplit">
            <div><span className="k">Closed</span><Money v={m.realised} /></div>
            <div><span className="k">Open</span><Money v={m.open} /></div>
            <div><span className="k">Trades</span><span>{m.trades || 0}{m.trades ? ` · ${Math.round(100 * m.won / m.trades)}% won` : ""}</span></div>
          </div>
        </div>; })}
    </section>
    <p className="note">Closed is after charges; open is marked at the last price, before exit charges. Totals cover the live
      strategies' closed trades and every gold or silver position open on Zerodha now. Updated {data.at} IST.</p>
    {data.errors?.map(e => <div className="msg err" key={e}>{e}</div>)}

    <section className="block">
      <div className="bhead"><h2>Strategies</h2></div>
      <div className="strats">{(data.strategies || []).map(s => <div className={"strat" + (s.halted ? " halted" : "")} key={s.name}>
        <Metal m={s.metal} /><span className="mono">{s.name}</span>
        <span className="sstat">{s.halted ? `HALTED: ${s.halted}` : s.status}</span>
        {s.working && <span className="dim small">working: {s.working}</span>}
        {s.since && <span className="dim small">live since {s.since}</span>}
      </div>)}</div>
    </section>

    <div className="mfilter" role="radiogroup" aria-label="Metal">
      {[["all", "All"], ...Object.entries(METALS)].map(([k, name]) =>
        <button type="button" key={k} role="radio" aria-checked={only === k} onClick={() => setOnly(k)}>{name}</button>)}
    </div>

    <section className="block">
      <div className="bhead"><h2>Open positions</h2><span className="count">{positions.length}</span></div>
      <div className="sheet"><table>
        <thead><tr><th>Metal</th><th>Contract</th><th>By</th><th>Side</th><th className="n">Lots</th><th className="n">Entry</th>
          <th className="n">LTP</th><th className="n">P&amp;L</th></tr></thead>
        <tbody>{!positions.length ? <tr><td colSpan={8} className="empty">Flat</td></tr> : positions.map(p =>
          <tr key={`${p.contract} ${p.product} ${p.strategy}`}>
            <td><Metal m={p.metal} /></td><td className="mono">{p.contract} <span className="dim">{p.product}</span></td>
            <td>{p.strategy ? <span className="by">{p.strategy}</span> : <span className="dim">other</span>}</td>
            <td><Side s={p.side} /></td><td className="n">{p.lots}</td>
            <td className="n">{px(p.entry)}{p.entry_at && <div className="dim small">{p.entry_at}</div>}</td>
            <td className="n">{px(p.ltp)}</td><td className="n"><Money v={p.pnl} strong /></td>
          </tr>)}</tbody>
      </table></div>
    </section>

    <section className="block">
      <div className="bhead"><h2>Closed trades</h2><span className="count">{trades.length}</span></div>
      <div className="sheet"><table>
        <thead><tr><th>Metal</th><th>Contract</th><th>Strategy</th><th>Side</th><th className="n">Lots</th><th className="n">Entry</th>
          <th className="n">Exit</th><th>Why</th><th className="n">Gross</th><th className="n">Charges</th><th className="n">Net</th></tr></thead>
        <tbody>{!trades.length ? <tr><td colSpan={11} className="empty">No closed trades yet</td></tr> : trades.map((t, i) =>
          <tr key={i}>
            <td><Metal m={t.metal} /></td><td className="mono">{t.contract}</td><td><span className="by">{t.strategy}</span></td>
            <td><Side s={t.side} /></td><td className="n">{t.lots}</td>
            <td className="n">{px(t.entry)}<div className="dim small">{t.entry_at}</div></td>
            <td className="n">{px(t.exit)}<div className="dim small">{t.exit_at}</div></td>
            <td className="dim">{t.reason}</td>
            <td className="n"><Money v={t.gross} /></td><td className="n dim">{rupees(t.charges)}</td>
            <td className="n"><Money v={t.net} strong /></td>
          </tr>)}</tbody>
      </table></div>
    </section>
  </>;
}
