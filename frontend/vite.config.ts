import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// In development the browser talks to the Vite dev server only, and Vite
// forwards /api to the Go backend. In production nginx does the same job
// (see nginx.conf.template), so the app always calls same-origin URLs.
export default defineConfig({
  plugins: [react()],
  server: {
    host: true,
    port: 5173,
    proxy: {
      "/api": process.env.BACKEND_URL ?? "http://localhost:8080",
    },
    // File events do not cross bind mounts on Windows and macOS
    watch: { usePolling: true, interval: 300 },
  },
});
