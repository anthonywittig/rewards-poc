import { ApiError } from './types'

export function formatWhen(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toLocaleString(undefined, {
    year: 'numeric',
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  })
}

export function formatDate(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toLocaleDateString(undefined, {
    year: 'numeric',
    month: 'short',
    day: 'numeric',
  })
}

export function tierLabel(level: string): string {
  return level.charAt(0).toUpperCase() + level.slice(1)
}

export function errorMessage(err: unknown): string {
  if (err instanceof ApiError) return err.message
  if (err instanceof Error) return err.message
  return 'Something went wrong'
}

export function errorCode(err: unknown): string | undefined {
  if (err instanceof ApiError) return err.code
  return undefined
}

/** Build a Temporal visibility query from the UI chips + the name search box. */
export function buildListQuery(opts: {
  tier: string | null
  status: 'active' | 'deactivated' | 'any'
  name: string
}): string {
  const parts: string[] = []
  if (opts.tier) {
    parts.push(`RewardsLevel = '${opts.tier}'`)
  }
  if (opts.status === 'active') {
    parts.push(`RewardsActive = true`)
  } else if (opts.status === 'deactivated') {
    parts.push(`RewardsActive = false`)
  }
  const name = opts.name.trim()
  if (name) {
    // CustomerName is registered as Text, so this is a tokenized match: whole
    // words, not prefixes. "lovelace" finds Ada Lovelace, "lovel" does not.
    parts.push(`CustomerName = '${escapeQueryLiteral(name)}'`)
  }
  return parts.join(' AND ')
}

/** Escape a user-typed value for a single-quoted visibility-query literal. */
export function escapeQueryLiteral(value: string): string {
  return value.replace(/\\/g, '\\\\').replace(/'/g, "\\'")
}

export function slugifyId(name: string): string {
  return name
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 40) || `c-${Date.now().toString(36)}`
}
