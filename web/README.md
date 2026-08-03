# Rewards web UI

React app over the rewards API.

```sh
# from repo root — `make up` brings up the whole stack, this included
make logs SVC=web  # what the dev server is saying, on :5173
```

The dev server starts last in `make up` and nothing waits on it, because a first
start installs a few hundred npm packages over a bind mount. Until that finishes
`:5173` refuses connections; `make logs SVC=web` shows where it is.

The dev server runs in Compose off the `node` image, with this directory
bind-mounted, so an edit reloads without rebuilding anything. `node_modules`
lives in a named volume rather than the bind mount, so a host-side `npm install`
and the container's never overwrite each other.

To typecheck / production-build (the dev server does not):

```sh
docker compose -f deploy/docker-compose.yml exec -T web npm run build
```

Browser requests stay same-origin. Vite proxies `/api` to
`VITE_API_PROXY_TARGET`, which the service sets to `http://api:8081` — the proxy
hop happens inside the compose network, not in the browser. Links to the Temporal
UI use `VITE_TEMPORAL_UI_URL`, which is a published address (`localhost`) because
the browser is the one following it. The dev server's own port follows `WEB_PORT`
(default `5173`, `strictPort`), published unchanged so Vite's hot-reload socket
reaches the same port the browser loaded from. The Go API does not send CORS
headers, so pointing the browser at `:8081` directly fails — use the proxy.
