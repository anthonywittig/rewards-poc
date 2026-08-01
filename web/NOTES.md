# Web UI notes (Phase 8)

Local README for the Vite app. These are Phase 8 handoff notes, kept for the
record — the draft paragraph below has since landed in the project README, and
the correction finding 1 asks for has landed in PLAN.md §9.

> The React UI lives in `web/`. Run `make api` then `make web` (Vite on
> `:5173`; `/api` is proxied to the API on `:8081`). Override with
> `VITE_API_PROXY_TARGET` if needed.

## Findings for PLAN.md

> Integrator: splice into §12. Do not renumber from this branch.

1. **Create form needs an explicit `customerId` field.** §9 only mentions name +
   email, but `EnrollRequest` requires `customerId` and the workflow ID is
   `customer-<id>`. The UI auto-slugs from the name and lets you edit it. Worth
   a one-line correction in §9 so the create screen matches the frozen contract.
   *(Done — §9 now lists the `customerId` field.)*

2. **Status chips map to `RewardsActive`, not `ExecutionStatus`.** Soft-inactive
   customers stay `Running`, so the list query uses `RewardsActive = true|false`
   while detail payloads say `active`|`deactivated`. The UI translates between them.

3. **Optimistic list insert is session-scoped with a TTL.** §9 says the list should
   optimistically insert after create. Implemented via `sessionStorage` (cleared once
   the server list includes that ID, or after ~2s past the lag window). Filtered against
   the active visibility query so a basic enroll does not appear under a gold filter.

4. **Real API needs a Vite proxy, not a cross-origin base URL.** The Go API does not
   send CORS headers. `VITE_API_BASE=http://localhost:8081` fails in the browser.
   Use the Vite proxy (default `:8081`) so requests stay same-origin.
