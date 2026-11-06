import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import App, { BrokerCard } from "./App.jsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const card = (broker) =>
  render(<BrokerCard broker={{ name: "upstox", label: "Upstox", ...broker }} onLogin={() => {}} onLogout={() => {}} />);

describe("BrokerCard", () => {
  it("offers login when logged out", () => {
    card({ state: "logged_out" });
    expect(screen.getByRole("button", { name: "Log in to Upstox" })).toBeTruthy();
  });

  it("shows the user and an IST expiry when active, and offers logout", () => {
    card({ state: "active", user_id: "AB1234", expires_at: "2026-11-06T22:00:00Z" });
    expect(screen.getByText(/AB1234/).textContent).toMatch(/03:30 IST/);
    expect(screen.getByRole("button", { name: "Log out" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Log in/ })).toBeNull();
  });

  it("offers login again when expired", () => {
    card({ state: "expired", user_id: "AB1234", expires_at: "2026-11-06T22:00:00Z" });
    expect(screen.getByText("Expired")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Log in to Upstox" })).toBeTruthy();
  });

  it("offers no action when not configured", () => {
    card({ state: "not_configured" });
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("shows a broker error", () => {
    card({ state: "logged_out", error: "upstox refused the login" });
    expect(screen.getByRole("alert").textContent).toBe("upstox refused the login");
  });
});

describe("login", () => {
  const brokers = [{ name: "zerodha", label: "Zerodha", state: "logged_out", token_param: "request_token" }];
  const url = "https://kite.zerodha.com/connect/login?v=3&api_key=k";

  function stubApi(tokenResponse) {
    const calls = [];
    vi.stubGlobal("fetch", async (path, opts) => {
      calls.push({ path, body: opts?.body });
      if (path.endsWith("/login")) return { ok: true, json: async () => ({ url }) };
      if (path.endsWith("/token")) return tokenResponse;
      return { ok: true, json: async () => brokers };
    });
    return calls;
  }

  it("shows the URL and a token box, then submits the token", async () => {
    const calls = stubApi({
      ok: true,
      json: async () => [{ ...brokers[0], state: "active", user_id: "ZD1", expires_at: "2026-11-07T00:30:00Z" }],
    });
    render(<App />);
    fireEvent.click(await screen.findByRole("button", { name: "Log in to Zerodha" }));
    const link = await screen.findByRole("link");
    expect(link.getAttribute("href")).toBe(url);
    const box = screen.getByLabelText(/request_token/);
    expect(screen.getByRole("button", { name: "Submit" }).disabled).toBe(true);
    fireEvent.change(box, { target: { value: "abc123" } });
    fireEvent.click(screen.getByRole("button", { name: "Submit" }));
    expect(await screen.findByText(/ZD1/)).toBeTruthy();
    expect(JSON.parse(calls.find((c) => c.path.endsWith("/token")).body)).toEqual({ value: "abc123" });
    expect(screen.queryByRole("link")).toBeNull();
  });

  it("keeps the box open after a refused token so it can be corrected", async () => {
    stubApi({ ok: false, status: 502, json: async () => ({ error: "zerodha refused the login" }) });
    render(<App />);
    fireEvent.click(await screen.findByRole("button", { name: "Log in to Zerodha" }));
    fireEvent.change(await screen.findByLabelText(/request_token/), { target: { value: "bad" } });
    fireEvent.click(screen.getByRole("button", { name: "Submit" }));
    expect((await screen.findByRole("alert")).textContent).toMatch(/refused/);
    expect(screen.getByLabelText(/request_token/).value).toBe("bad");
  });
});

describe("App", () => {
  it("lists brokers from the API", async () => {
    vi.stubGlobal("fetch", async () => ({
      ok: true,
      json: async () => [
        { name: "upstox", label: "Upstox", state: "logged_out" },
        { name: "zerodha", label: "Zerodha", state: "not_configured" },
      ],
    }));
    render(<App />);
    expect(await screen.findByText("Zerodha")).toBeTruthy();
    expect(screen.getByText("Upstox")).toBeTruthy();
  });

  it("says so when the server has gone", async () => {
    vi.stubGlobal("fetch", async () => Promise.reject(new Error("offline")));
    render(<App />);
    expect((await screen.findByRole("alert")).textContent).toMatch(/Run it again/);
  });
});
