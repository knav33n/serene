import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import { fileURLToPath } from "node:url";

// Two pages from one build: index.html (serene manual, 127.0.0.1:8767) and
// login.html (serene auth, 127.0.0.1:8080). `npm run dev` passes /api to the
// login page's server; VITE_API=http://127.0.0.1:8767 for the manual page's.
const api = process.env.VITE_API || "http://127.0.0.1:8080";
const page = name => fileURLToPath(new URL(name, import.meta.url));

export default defineConfig({
  plugins: [react()],
  build: { rollupOptions: { input: { index: page("index.html"), login: page("login.html") } } },
  server: {
    // The Go servers only accept their own Host and Origin.
    proxy: { "/api": { target: api, changeOrigin: true, headers: { Origin: api } } },
  },
  test: { environment: "jsdom" },
});
