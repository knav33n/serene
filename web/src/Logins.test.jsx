import { afterEach, expect, test, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import Logins from "./Logins.jsx";

const later = h => new Date(Date.now() + h * 3600e3).toISOString();
const brokers = [
  { name: "zerodha", label: "Zerodha", token_param: "request_token", state: "active", user_id: "ZD1",
    issued_at: later(-2), expires_at: later(10), pending: false,
    sent: [{ to: "calm", state: "ok" }, { to: "hearth", state: "failed", error: "hearth isn't running" }] },
  { name: "upstox", label: "Upstox", token_param: "code", state: "logged_out", pending: true },
  { name: "breeze", label: "ICICI Breeze", token_param: "apisession", state: "expired", user_id: "B1", pending: false },
  { name: "fyers", label: "Fyers", token_param: "auth_code", state: "not_configured", pending: false },
];

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

test("says what's left to do, and where each login went", async () => {
  const fetch = vi.fn(async () => new Response(JSON.stringify({ ok: true, result: brokers }),
    { headers: { "Content-Type": "application/json" } }));
  vi.stubGlobal("fetch", fetch);
  render(<Logins />);
  await waitFor(() => expect(screen.getByRole("heading", { level: 1 }).textContent)
    .toBe("Upstox and ICICI Breeze still need today's login."));
  expect(screen.getByText(/Logged in as/).textContent).toMatch(/ZD1, (9h 5\dm|10h 00m) left/);
  expect(screen.getByText("Sent to project_calm here")).toBeTruthy();
  expect(screen.getByText(/Not sent to hearth: hearth isn't running/)).toBeTruthy();
  expect(screen.getByRole("button", { name: "Send again" })).toBeTruthy();
  // Upstox's login is open: the paste field, not a second Log in
  expect(screen.getByLabelText("Upstox code")).toBeTruthy();
  expect(screen.getAllByRole("button", { name: "Log in" })).toHaveLength(1); // Breeze's
  expect(screen.getByText(/Not set up/)).toBeTruthy();
  expect(fetch.mock.calls[0][0]).toBe("api/brokers");
});
