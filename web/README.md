# Rewards web UI

Phase 8 React app. See [NOTES.md](./NOTES.md) for findings and the README blurb for Phase 9.

```sh
# from repo root
make mockapi   # :8082
make web       # :5173 → http://localhost:8082

# real API instead
VITE_API_BASE=http://localhost:8081 make web
```
