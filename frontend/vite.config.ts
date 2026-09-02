import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// go:embed (internal/backup/webui/webui.go) can only embed paths at or
// below its own package directory, so the build output is pointed there
// directly rather than left at frontend/dist and copied afterward.
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: "../internal/backup/webui/dist",
    emptyOutDir: true,
  },
  server: {
    proxy: {
      // Lets `npm run dev` drive a real `go run ./cmd/go-backup-tool -listen
      // 127.0.0.1:8080` backend with no CORS handling needed — the browser
      // only ever talks to the Vite dev server's own origin.
      "/api": "http://localhost:8080",
      "/login": "http://localhost:8080",
    },
  },
});
