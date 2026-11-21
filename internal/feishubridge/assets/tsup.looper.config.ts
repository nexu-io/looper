import { defineConfig } from 'tsup';

export default defineConfig({
  entry: ['src/looper-entry.ts'],
  outDir: 'dist-looper',
  format: ['esm'], target: 'node20', platform: 'node',
  clean: true, splitting: false, dts: false,
  external: ['@anthropic-ai/claude-agent-sdk'],
});
