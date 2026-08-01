# Rewards web UI

React app over the rewards API. See [docs/FINDINGS.md](../docs/FINDINGS.md).

```sh
# from repo root — `make up` brings up the stack, worker and API
make web       # :5173 — /api proxied to the API on :8081
```

Browser requests stay same-origin. Vite proxies `/api` (and `/healthz`) to
`VITE_API_PROXY_TARGET` (default `http://localhost:8081`); `make web` sets it
from the stack's env file so it follows `API_PORT`. Links to the Temporal UI
use `VITE_TEMPORAL_UI_URL` (default `http://localhost:8080`), likewise set from
`TEMPORAL_UI_PORT`, and the dev server's own port follows `WEB_PORT` (default
`5173`, `strictPort`). The Go API does not send CORS headers, so pointing the
browser at `:8081` directly fails — use the proxy.
