import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The build is embedded in the Go binary (internal/demo, go:embed) and served under /demo/.
// `npm run dev` proxies the demo API to a local `make demo-ui` server.
export default defineConfig({
  plugins: [react()],
  base: "/demo/",
  build: { outDir: "../internal/demo/web", emptyOutDir: true },
  server: {
    proxy: { "/demo/api": "http://127.0.0.1:8095" },
  },
});
