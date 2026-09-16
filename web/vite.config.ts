import { defineConfig } from "vite";

export default defineConfig({
  // Relative asset URLs: the bundle is served from ktestd's embedded
  // filesystem at whatever root the browser lands on.
  base: "./",
  worker: {
    format: "es",
    rollupOptions: { output: { entryFileNames: "assets/[name].js" } },
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
    // ktestd embeds dist/, so fixed asset names keep the committed tree stable
    // across rebuilds and the diff readable when the bundle changes.
    rollupOptions: {
      output: {
        entryFileNames: "assets/app.js",
        chunkFileNames: "assets/[name].js",
        assetFileNames: "assets/[name][extname]",
      },
    },
  },
});
