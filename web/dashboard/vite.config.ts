import { defineConfig } from "vite";
import vue from "@vitejs/plugin-vue";

const controllerTarget = process.env.ASTERFERRY_DASHBOARD_TARGET || "https://127.0.0.1:8443";
const dashboardOutput = process.env.ASTERFERRY_DASHBOARD_OUT || "../../internal/dashboard/dist";

export default defineConfig({
  base: "/dashboard/",
  plugins: [vue()],
  server: {
    host: "127.0.0.1",
    proxy: {
      "/api/v1": {
        target: controllerTarget,
        changeOrigin: true,
        // `controller init` creates a local CA-signed certificate.  The
        // browser still talks to Vite over localhost; proxying must not fail
        // solely because that development certificate is self-signed from
        // Node's perspective.
        secure: false,
      },
    },
  },
  build: {
    outDir: dashboardOutput,
    emptyOutDir: true,
    sourcemap: false,
  },
  test: {
    environment: "happy-dom",
  },
});
