import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import App from "./App.jsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const view = {
  expiries: ["2026-11-24", "2026-12-01"],
  free: { zerodha: { free: 500000 }, upstox: { free: null, error: "not logged in" } },
  positions: {
    zerodha: { rows: [{ symbol: "NIFTY26NOV25400CE", name: "NIFTY26NOV25400CE", exchange: "NFO", product: "MIS", qty: 65, avg: 100, ltp: 104, pnl: 260 }] },
    upstox: { rows: [], error: "not logged in" },
  },
  trades: {
    rows: [{ broker: "zerodha", symbol: "NIFTY26NOV25400CE", name: "NIFTY26NOV25400CE", exchange: "NFO", product: "MIS",
      qty: 65, bought: 65, buy_avg: 100, sold: 0, sell_avg: 0, carried: 0, ltp: 104, pnl: 260, charges: 25, net: 235,
      by: "manual", manual: true, target: 104.4,
      orders: [{ order_id: "1", time: "09:30:00", side: "BUY", qty: 65, price: 100, charges: 25 }] }],
    totals: { zerodha: { pnl: 260, charges: 25, net: 235, orders: 1, priced: 1 } },
    errors: { upstox: "not logged in" },
  },
};

function serve(handlers) {
  const calls = [];
  vi.stubGlobal("fetch", vi.fn(async (path, init) => {
    calls.push({ path, body: JSON.parse(init.body), headers: init.headers });
    if (path === "api/session" && !handlers[path]) handlers[path] = () => ({ gated: false, in: true });
    const result = handlers[path]?.(JSON.parse(init.body));
    return { ok: true, headers: { get: () => "application/json" }, json: async () => ({ ok: true, result }) };
  }));
  return calls;
}

describe("App", () => {
  it("has no login: it loads the desk at once, as JSON", async () => {
    const calls = serve({ "api/manual": () => view });
    render(<App />);
    expect(await screen.findByText("2026-11-24 · nearest")).toBeTruthy();
    expect(screen.queryByPlaceholderText("password")).toBeNull();
    expect(screen.queryByRole("button", { name: /Log/ })).toBeNull();
    expect(calls.find(c => c.path === "api/manual").headers["Content-Type"]).toBe("application/json");
    expect(screen.queryByRole("button", { name: "Log out" })).toBeNull();
    expect(screen.getByText("target 104.40")).toBeTruthy();
    expect(screen.getByText("₹5,00,000 free")).toBeTruthy();
  });

  it("buys on the brokers ticked after a confirm", async () => {
    const calls = serve({
      "api/manual": () => view,
      "api/manual/buy": () => ({ trades: [{ broker: "zerodha", contract: "NIFTY 2026-11-24 25400 CE", qty: 65, entry: 100, target: 104.4 }], errors: [], warnings: [] }),
    });
    vi.stubGlobal("confirm", vi.fn(() => true));
    render(<App />);
    await screen.findByText("2026-11-24 · nearest");
    fireEvent.change(screen.getByPlaceholderText("22750"), { target: { value: "25400" } });
    fireEvent.change(screen.getByPlaceholderText("market"), { target: { value: "100" } });
    fireEvent.click(screen.getByRole("checkbox", { name: /Upstox/ }));
    fireEvent.click(screen.getByRole("button", { name: "Buy" }));
    await screen.findByText(/bought 65 NIFTY 2026-11-24 25400 CE on Zerodha @ 100.00/);
    const buy = calls.find(c => c.path === "api/manual/buy").body;
    expect(buy).toEqual({ expiry: "2026-11-24", strike: 25400, option_type: "CE", limit: 100, target_pct: 2, capital: 700000, brokers: ["zerodha"] });
    expect(confirm.mock.calls[0][0]).toMatch(/^REAL order on Zerodha: buy NIFTY 2026-11-24 25400 CE with a limit at 100.00/);
    expect(confirm.mock.calls[0][0]).toMatch(/half to 25400 CE \(ATM\), half to 25300 CE \(ITM\)/);
  });

  it("sends nothing when the confirm is declined", async () => {
    const calls = serve({ "api/manual": () => view });
    vi.stubGlobal("confirm", vi.fn(() => false));
    render(<App />);
    await screen.findByText("2026-11-24 · nearest");
    fireEvent.click(screen.getByRole("button", { name: "Exit" }));
    await waitFor(() => expect(confirm).toHaveBeenCalled());
    expect(calls.some(c => c.path === "api/manual/exit")).toBe(false);
  });

  it("Sell now squares off the manual trade's position", async () => {
    const calls = serve({ "api/manual": () => view, "api/manual/exit": () => ({ symbol: "NIFTY26NOV25400CE", qty: 65, price: 104 }) });
    vi.stubGlobal("confirm", vi.fn(() => true));
    render(<App />);
    fireEvent.click(await screen.findByRole("button", { name: "Sell now" }));
    await screen.findByText(/squared off 65 NIFTY26NOV25400CE on Zerodha @ 104.00/);
    expect(calls.find(c => c.path === "api/manual/exit").body).toEqual({ broker: "zerodha", symbol: "NIFTY26NOV25400CE", exchange: "NFO", product: "MIS" });
  });
});

describe("brokers", () => {
  it("offers Fyers and ICICI, unticked until chosen", async () => {
    const calls = serve({
      "api/manual": () => view,
      "api/manual/buy": () => ({ trades: [{ broker: "icici", contract: "NIFTY 2026-11-24 25400 CE", qty: 65, entry: 100, target: 104.4 }], errors: [], warnings: [] }),
    });
    vi.stubGlobal("confirm", vi.fn(() => true));
    render(<App />);
    await screen.findByText("2026-11-24 · nearest");
    expect(screen.getByRole("checkbox", { name: /Fyers/ }).checked).toBe(false);
    expect(screen.getByRole("checkbox", { name: /ICICI/ }).checked).toBe(false);
    fireEvent.change(screen.getByPlaceholderText("22750"), { target: { value: "25400" } });
    fireEvent.click(screen.getByRole("checkbox", { name: /Zerodha/ }));
    fireEvent.click(screen.getByRole("checkbox", { name: /Upstox/ }));
    fireEvent.click(screen.getByRole("checkbox", { name: /ICICI/ }));
    fireEvent.click(screen.getByRole("button", { name: "Buy" }));
    await screen.findByText(/bought 65 NIFTY 2026-11-24 25400 CE on ICICI/);
    expect(calls.find(c => c.path === "api/manual/buy").body.brokers).toEqual(["icici"]);
    expect(confirm.mock.calls[0][0]).toMatch(/^REAL order on ICICI: buy/);
  });
});

describe("gate", () => {
  it("asks for the password and code first when gated, then loads", async () => {
    let inside = false;
    const calls = [];
    vi.stubGlobal("fetch", vi.fn(async (path, init) => {
      const body = JSON.parse(init.body);
      calls.push({ path, body });
      const reply = (status, j) => ({ ok: status === 200, status, statusText: "", headers: { get: () => "application/json" }, json: async () => j });
      if (path === "api/session") return reply(200, { ok: true, result: { gated: true, in: inside } });
      if (path === "api/login") {
        if (body.code !== "123456") return reply(401, { ok: false, error: "wrong password or code" });
        inside = true;
        return reply(200, { ok: true, result: null });
      }
      if (path === "api/manual") return inside ? reply(200, { ok: true, result: view }) : reply(401, { ok: false, error: "log in first" });
      return reply(200, { ok: true, result: null });
    }));
    render(<App />);
    const pw = await screen.findByLabelText("Password");
    expect(calls.some(c => c.path === "api/manual")).toBe(false);
    fireEvent.change(pw, { target: { value: "correct horse battery" } });
    fireEvent.change(screen.getByLabelText("Code"), { target: { value: "111111" } });
    fireEvent.click(screen.getByRole("button", { name: "Unlock" }));
    expect(await screen.findByText("wrong password or code")).toBeTruthy();
    fireEvent.change(screen.getByLabelText("Code"), { target: { value: "12 34 56" } });
    fireEvent.click(screen.getByRole("button", { name: "Unlock" }));
    expect(await screen.findByText("2026-11-24 · nearest")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Log out" }));
    expect(await screen.findByLabelText("Password")).toBeTruthy();
    expect(calls.some(c => c.path === "api/logout")).toBe(true);
  });

  const card = { since: "2026-11-25", days: 4, net: 3110.62, brokers: { zerodha: -1000, upstox: 4110.62 } };

  it("shows the running total by broker above the form", async () => {
    serve({ "api/manual": () => ({ ...view, trading_day: true, traded: false, card }) });
    render(<App />);
    expect(await screen.findByText("Made since 25 Nov")).toBeTruthy();
    expect(screen.getByText("₹3,111")).toBeTruthy(); // the sum
    expect(screen.getByText("₹4,111")).toBeTruthy();
    expect(screen.getByText("−₹1,000")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Buy" })).toBeTruthy();
  });

  it("hides the form once the day's trade is taken, keeping positions and today", async () => {
    serve({ "api/manual": () => ({ ...view, trading_day: true, traded: true, card }) });
    render(<App />);
    expect(await screen.findByText(/Today's trade is taken/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Buy" })).toBeNull();
    expect(screen.queryByPlaceholderText("22750")).toBeNull();
    expect(screen.getByText("Positions")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Sell now" })).toBeTruthy();
  });

  it("shows only the card on a holiday", async () => {
    serve({ "api/manual": () => ({ ...view, trading_day: false, traded: false, card }) });
    render(<App />);
    expect(await screen.findByText("Made since 25 Nov")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Buy" })).toBeNull();
    expect(screen.queryByText("Positions")).toBeNull();
    expect(screen.queryByText("Today")).toBeNull();
    expect(screen.queryByText("Day net")).toBeNull();
  });
});
