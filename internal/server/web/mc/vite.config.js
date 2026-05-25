import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import path from "path";

export default defineConfig({
  plugins: [react()],
  base: "/static/mc/",
  build: {
    outDir: path.resolve(__dirname, "../static/mc"),
    emptyOutDir: true,
  },
});
