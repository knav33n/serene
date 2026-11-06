import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

const api = "http://127.0.0.1:8080";

export default defineConfig({
  plugins: [react()],
  server: {
    // The Go server only accepts its own Host and Origin.
    proxy: { "/api": { target: api, changeOrigin: true, headers: { Origin: api } } },
  },
  test: { environment: "jsdom" },
});
