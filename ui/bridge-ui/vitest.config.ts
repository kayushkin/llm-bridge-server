import { defineConfig } from 'vitest/config'

// `@kayushkin/chat-core` is a file: link with its own node_modules, so without
// this a context it creates is built by ITS copy of react and read by ours —
// `useContext` on a null dispatcher. Same rule the hosts' vite configs carry.
export default defineConfig({
  resolve: { dedupe: ['react', 'react-dom'] },
  test: { environment: 'node', include: ['test/**/*.test.ts'] },
})
