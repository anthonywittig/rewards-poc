# Rewards web UI

React app over the rewards API. See [docs/FINDINGS.md](../docs/FINDINGS.md).

```sh
# from repo root — `make up` brings up the whole stack, this included
make web-logs  # what the dev server is saying, on :5173
make web       # restart it (rarely needed: web/ is bind-mounted, edits hot-reload)
```

The dev server runs in Compose off the `node` image, with this directory
bind-mounted, so an edit reloads without rebuilding anything. `node_modules`
lives in a named volume rather than the bind mount, so a host-side `npm install`
and the container's never overwrite each other.

Browser requests stay same-origin. Vite proxies `/api` (and `/healthz`) to
`VITE_API_PROXY_TARGET`, which the service sets to `http://api:8081` — the proxy
hop happens inside the compose network, not in the browser. Links to the Temporal
UI use `VITE_TEMPORAL_UI_URL`, which is a published address (`localhost`) because
the browser is the one following it. The dev server's own port follows `WEB_PORT`
(default `5173`, `strictPort`), published unchanged so Vite's hot-reload socket
reaches the same port the browser loaded from. The Go API does not send CORS
headers, so pointing the browser at `:8081` directly fails — use the proxy.
