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

/**
 * Title-case a tier for display. Tier values travel lowercase everywhere else
 * -- API payloads, visibility queries, CSS class names -- so every place that
 * puts one on screen goes through here and nowhere else.
 */
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
