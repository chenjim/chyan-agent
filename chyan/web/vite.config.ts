// Vite config: relative base so the SPA works when mounted at any path prefix.
import { defineConfig } from 'vitest/config';

export default defineConfig({
  base: './',
  build: {
    target: 'es2020',
    outDir: 'dist',
    emptyOutDir: true,
    // Keep a tiny bundle; single-binary embed prefers small assets.
    chunkSizeWarningLimit: 256,
  },
  test: {
    environment: 'node',
    include: ['src/**/*.test.ts'],
    coverage: {
      provider: 'v8',
      include: ['src/**/*.ts'],
      exclude: ['src/**/*.test.ts', 'src/types.ts'],
      reporter: ['text', 'html'],
    },
  },
});
