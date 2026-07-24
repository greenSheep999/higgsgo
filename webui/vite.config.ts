import path from "node:path";
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

const adminProxyTarget = process.env.HIGGSGO_ADMIN_PROXY ?? "http://127.0.0.1:18081";

// Vite's shorthand form (`"/admin": "http://..."`) leans on
// http-proxy-middleware defaults that, in some versions, drop the
// Authorization header on WebSocket-eligible routes. Spell the proxy
// config out explicitly and force header forwarding so the SPA's
// bearer survives the hop from :5373 → :18081. See webui README §Auth.
const adminProxy = {
  target: adminProxyTarget,
  changeOrigin: true,
  ws: false,
  configure: (proxy: any) => {
    proxy.on("proxyReq", (proxyReq: any, req: any) => {
      const auth = req.headers?.authorization;
      if (auth) proxyReq.setHeader("Authorization", auth);
    });
  },
};

// Dev server proxies /admin/* and /v1/playground/* to the higgsgo admin
// listener so we can hit the API from the same origin the SPA is served on.
// In prod the SPA is embedded via //go:embed and served by the same
// listener, so there is no CORS or proxy concern.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "./src"),
    },
    // Force a single instance of React across the graph. Without this,
    // pre-bundling can hand different resolved paths to different callers,
    // and named exports (useContext, useState…) go missing when Vite
    // treats React's CJS entry as a bag-of-exports.
    dedupe: ["react", "react-dom"],
  },
  optimizeDeps: {
    // Include hook consumers explicitly so Vite pre-bundles them against
    // the same React copy the app uses. Without this list, packages that
    // import React lazily can trigger a second scan pass whose CJS→ESM
    // interop drops named exports and yields "useContext of null".
    include: [
      "react",
      "react-dom",
      "react-dom/client",
      "react-i18next",
      "next-themes",
    ],
  },
  server: {
    // 5373 is deliberately non-standard so a sibling project's vite
    // dev server (e.g. claudego's, which uses 5273) can't collide.
    // Vite's auto-increment fallback silently jumps to 5174/5175
    // when 5173 is taken and the browser tab you already opened
    // then serves someone else's SPA — this bit us in P4-3c.
    // strictPort makes the collision fail loudly instead.
    port: 5373,
    strictPort: true,
    proxy: {
      "/admin": adminProxy,
      "/v1/playground": adminProxy,
      // /v1/models is mirrored on the admin listener (behind admin
      // bearer) so the WebUI's group model picker can enumerate the
      // catalog through the same base URL as every other admin
      // request. Without this proxy line the request lands on the
      // vite dev server and gets the SPA's index.html handed back —
      // a 200 with HTML, which then explodes JSON parsing downstream.
      "/v1/models": adminProxy,
    },
  },
});
