import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import App from "./App.jsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const mcx = { metals: { gold: { net: 15500 }, silver: { net: 0 } }, strategies: [], positions: [], trades: [] };
const reply = (status, j) => ({ ok: status === 200, status, statusText: "", headers: { get: () => "application/json" }, json: async () => j });

describe("App", () => {
  it("has no login: it loads Gold & Silver at once, as JSON, and no NIFTY desk", async () => {
    const calls = [];
    vi.stubGlobal("fetch", vi.fn(async (path, init) => {
      calls.push({ path, headers: init.headers });
      const result = path === "api/session" ? { gated: false, in: true } : path === "api/mcx" ? mcx : null;
      return reply(200, { ok: true, result });
    }));
    render(<App />);
    expect(await screen.findByText("₹15,500")).toBeTruthy();
    expect(screen.queryByPlaceholderText("password")).toBeNull();
    expect(screen.queryByRole("button", { name: "Log out" })).toBeNull();
    expect(screen.queryByText("NIFTY")).toBeNull();
    expect(calls.find(c => c.path === "api/mcx").headers["Content-Type"]).toBe("application/json");
    expect(calls.some(c => c.path.startsWith("api/manual"))).toBe(false);
  });
});

describe("gate", () => {
  it("asks for the password and code first when gated, then loads", async () => {
    let inside = false;
    const calls = [];
    vi.stubGlobal("fetch", vi.fn(async (path, init) => {
      const body = JSON.parse(init.body);
      calls.push({ path, body });
      if (path === "api/session") return reply(200, { ok: true, result: { gated: true, in: inside } });
      if (path === "api/login") {
        if (body.code !== "123456") return reply(401, { ok: false, error: "wrong password or code" });
        inside = true;
        return reply(200, { ok: true, result: null });
      }
      if (path === "api/mcx") return inside ? reply(200, { ok: true, result: mcx }) : reply(401, { ok: false, error: "log in first" });
      return reply(200, { ok: true, result: null });
    }));
    render(<App />);
    const pw = await screen.findByLabelText("Password");
    expect(calls.some(c => c.path === "api/mcx")).toBe(false);
    fireEvent.change(pw, { target: { value: "correct horse battery" } });
    fireEvent.change(screen.getByLabelText("Code"), { target: { value: "111111" } });
    fireEvent.click(screen.getByRole("button", { name: "Unlock" }));
    expect(await screen.findByText("wrong password or code")).toBeTruthy();
    fireEvent.change(screen.getByLabelText("Code"), { target: { value: "12 34 56" } });
    fireEvent.click(screen.getByRole("button", { name: "Unlock" }));
    expect(await screen.findByText("₹15,500")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Log out" }));
    expect(await screen.findByLabelText("Password")).toBeTruthy();
    expect(calls.some(c => c.path === "api/logout")).toBe(true);
  });
});
