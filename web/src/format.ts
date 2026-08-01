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

/** Build a Temporal visibility query from UI chips + optional raw override. */
export function buildListQuery(opts: {
  tier: string | null
  status: 'active' | 'deactivated' | 'any'
  raw: string
}): string {
  const raw = opts.raw.trim()
  if (raw) return raw

  const parts: string[] = []
  if (opts.tier) {
    parts.push(`RewardsLevel = '${opts.tier}'`)
  }
  if (opts.status === 'active') {
    parts.push(`RewardsActive = true`)
  } else if (opts.status === 'deactivated') {
    parts.push(`RewardsActive = false`)
  }
  return parts.join(' AND ')
}

export function slugifyId(name: string): string {
  return name
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 40) || `c-${Date.now().toString(36)}`
}
