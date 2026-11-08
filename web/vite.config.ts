import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The build goes into the Go package that embeds it. Only dist/app is
// written (and emptied); the committed placeholder dist/index.html is
// left alone.
export default defineConfig({
  plugins: [react()],
  base: "./",
  build: {
    outDir: "../internal/desktop/ui/dist/app",
    emptyOutDir: true,
    chunkSizeWarningLimit: 1024,
  },
});
