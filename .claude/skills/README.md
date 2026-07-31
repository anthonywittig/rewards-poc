# Vendored skills

Skills checked into this repo are available to anyone who clones it — no
per-developer install step. Claude Code picks up `SKILL.md` automatically and
reads files under `references/` on demand.

## temporal-developer

Temporal development guidance (workflows, activities, determinism, testing,
versioning) for Python, TypeScript, Go, Java, .NET, Ruby, and Rust.

| | |
|---|---|
| Upstream | https://github.com/temporalio/skill-temporal-developer |
| Skill version | 0.5.0 |
| Vendored at commit | `4f7b146` (2026-07-05) |
| License | MIT — see `temporal-developer/LICENSE` |

Vendored contents are `SKILL.md`, `references/`, and `LICENSE`. Upstream's
`README.md` (install instructions) and `.github/` were dropped as they don't
apply once the skill lives here.

### Updating

Re-copy from upstream rather than hand-editing, so local changes never drift
into something we can't re-sync:

```sh
git clone --depth 1 https://github.com/temporalio/skill-temporal-developer /tmp/tds
rm -rf .claude/skills/temporal-developer
mkdir -p .claude/skills/temporal-developer
cp /tmp/tds/SKILL.md /tmp/tds/LICENSE .claude/skills/temporal-developer/
cp -r /tmp/tds/references .claude/skills/temporal-developer/references
```

Then update the version and commit in the table above.
