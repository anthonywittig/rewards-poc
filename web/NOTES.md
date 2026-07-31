# Web UI notes (Phase 8)

Local README for the Vite app. Project-level README is owned by Phase 9 — do not
edit it from this phase. Draft paragraph for the integrator:

> The React UI lives in `web/`. Run `make mockapi` then `make web` (Vite on
> `:5173`; `/api` is proxied to the mock on `:8082`). For the real API:
> `VITE_API_PROXY_TARGET=http://localhost:8081 make web`.

## Findings for PLAN.md

> Integrator: splice into §12. Do not renumber from this branch.

1. **Create form needs an explicit `customerId` field.** §9 only mentions name +
   email, but `EnrollRequest` requires `customerId` and the workflow ID is
   `customer-<id>`. The UI auto-slugs from the name and lets you edit it. Worth
   a one-line correction in §9 so the create screen matches the frozen contract.

2. **Status chips map to Temporal `ExecutionStatus`, not the API's
   `status` string.** The list query uses `ExecutionStatus = 'Running'|'Canceled'`
   while detail payloads say `active`|`deactivated`. The UI translates; §9's
   "status toggle (Running/Canceled)" is the visibility vocabulary and is
   correct — just easy to confuse with the DTO field of the same English word.

3. **Optimistic list insert is session-scoped with a TTL.** §9 says the list should
   optimistically insert after create. Implemented via `sessionStorage` (cleared once
   the server list includes that ID, or after ~2s past the lag window). Filtered against
   the active visibility query so a basic enroll does not appear under a gold filter.

4. **Real API needs a Vite proxy, not a cross-origin base URL.** The Go API does not
   send CORS headers; only the mock does. `VITE_API_BASE=http://localhost:8081` fails in
   the browser. Use `VITE_API_PROXY_TARGET=http://localhost:8081` so Vite same-origin
   proxies `/api`. Worth a sentence in the Phase 9 README when it lands.
