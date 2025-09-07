import { defineConfig } from 'vite';

export default defineConfig({
  server: {
    port: 5178, // keep current if already used; adjust if needed
    proxy: {
      '/api': {
        target: 'http://localhost:8080',
        changeOrigin: true,
        ws: false,
      }
    }
  }
});
