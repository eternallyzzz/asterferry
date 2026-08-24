import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  base: "/dashboard/",
  plugins: [react()],
  build: {
    outDir: "../../internal/dashboard/dist",
    emptyOutDir: true,
    sourcemap: false,
  },
});
