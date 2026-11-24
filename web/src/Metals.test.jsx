import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import App from "./App.jsx";

afterEach(() => { cleanup(); vi.unstubAllGlobals(); location.hash = ""; });

const mcx = {
  at: "2026-11-24 15:00:00",
  metals: { gold: { realised: 12000, open: 3500, net: 15500, trades: 2, won: 1, lots: 1 },
            silver: { realised: -4000, open: 0, net: -4000, trades: 1, won: 0, lots: 0 } },
  strategies: [{ name: "goldm_hmasma_10m", metal: "gold", since: "24 Nov 2026 11:37", status: "long 1 lot GOLDM27JANFUT since 24 Nov 14:10" },
               { name: "silverm_hmasma_10m", metal: "silver", since: "24 Nov 2026 11:37", status: "flat", halted: "rejected" }],
  positions: [{ metal: "gold", strategy: "goldm_hmasma_10m", contract: "GOLDM27JANFUT", product: "NRML", side: "long", lots: 1,
                entry: 123000, entry_at: "24 Nov 14:10", ltp: 123350, pnl: 3500 }],
  trades: [{ metal: "silver", strategy: "silverm_hmasma_10m", contract: "SILVERM27FEBFUT", side: "short", lots: 1, entry: 150000,
             entry_at: "2026-11-24 10:00:00", exit: 150800, exit_at: "2026-11-24 12:00:00", reason: "cross", gross: -4000, charges: 60, net: -4060 }],
};

it("shows a card per metal and the positions and trades on #metals", async () => {
  location.hash = "#metals";
  vi.stubGlobal("fetch", vi.fn(async path => {
    const result = path === "api/session" ? { gated: false, in: true } : path === "api/mcx" ? mcx : null;
    return { ok: true, headers: { get: () => "application/json" }, json: async () => ({ ok: true, result }) };
  }));
  render(<App />);
  expect(await screen.findByText("₹15,500")).toBeTruthy();
  expect(screen.getByText("−₹4,000", { selector: ".mnet span" })).toBeTruthy();
  expect(screen.getByText("HALTED: rejected")).toBeTruthy();
  expect(screen.getAllByText("GOLDM27JANFUT", { exact: false }).length).toBeGreaterThan(0);
  fireEvent.click(screen.getByRole("radio", { name: "Gold" }));
  expect(screen.getByText("No closed trades yet")).toBeTruthy();
  expect(fetch.mock.calls.some(c => c[0] === "api/manual")).toBe(false);
});
