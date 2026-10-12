import { defineConfig } from "vite";
export default defineConfig({
  build: {
    outDir: "../internal/webui/static",
    emptyOutDir: true,
    // Preserve native light-dark() resolution against the used color-scheme.
    cssTarget: false,
  },
  server: {
    proxy: { "/api": { target: "http://localhost:8080", changeOrigin: false } },
  },
});
