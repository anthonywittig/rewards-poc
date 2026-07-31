# Rewards web UI

Phase 8 React app. See [NOTES.md](./NOTES.md) for findings and the README blurb for Phase 9.

```sh
# from repo root
make mockapi   # :8082
make web       # :5173 — /api proxied to the mock (no CORS needed)

# real API instead (stack + worker + api already up)
VITE_API_PROXY_TARGET=http://localhost:8081 make web
```

Browser requests stay same-origin. Vite proxies `/api` (and `/healthz`) to
`VITE_API_PROXY_TARGET` (default `http://localhost:8082`). The real Go API does
not send CORS headers, so pointing the browser at `:8081` directly fails — use
the proxy.
