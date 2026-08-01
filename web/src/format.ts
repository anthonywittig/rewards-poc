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
  // One clause per term, ANDed, so typing narrows instead of widening: the
  // `=` match this replaced ORs its words, and "ada turing" returned both Ada
  // Lovelace and Alan Turing.
  for (const term of nameTerms(opts.name)) {
    parts.push(`CustomerName STARTS_WITH '${escapeQueryLiteral(term)}'`)
  }
  return parts.join(' AND ')
}

/**
 * Split a name -- typed or stored -- into the lowercase terms a CustomerName
 * prefix search works in.
 *
 * The split mirrors Elasticsearch's standard tokenizer, which is what indexed
 * the field: punctuation and whitespace break tokens, but an intra-word
 * apostrophe does not ("Mary-Jane" is two tokens, "O'Brien" is one).
 *
 * Lowercased because indexed tokens are, and STARTS_WITH is a prefix match on
 * the stored token rather than an analyzed one -- "Lovel" finds nobody.
 */
export function nameTerms(input: string): string[] {
  return input
    .toLowerCase()
    .split(/[^\p{L}\p{N}']+/u)
    // Temporal's query literals do not round-trip an apostrophe -- neither
    // \' nor '' survives -- so cut each term at the first one. A shorter
    // prefix is still a correct prefix, it just matches more, which beats
    // "O'Brien" matching nothing at all.
    .map((term) => term.split("'")[0])
    .filter(Boolean)
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
