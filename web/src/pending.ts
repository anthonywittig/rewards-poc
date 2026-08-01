import { nameTerms } from './format'
import type { CustomerListItem, CustomerResponse } from './types'

const KEY = 'rewards.pendingList'
/** Longer than mock lag (400ms) and real ES lag (~300ms). After this, drop the optimistic row. */
const PENDING_TTL_MS = 2000

interface PendingEntry {
  customer: CustomerListItem
  at: number
}

/** Optimistically keep a just-created customer visible until ES catches up. */
export function rememberPending(customer: CustomerListItem): void {
  const next: PendingEntry[] = [
    { customer, at: Date.now() },
    ...readPendingEntries().filter((e) => e.customer.customerId !== customer.customerId),
  ]
  sessionStorage.setItem(KEY, JSON.stringify(next.slice(0, 20)))
}

function readPendingEntries(): PendingEntry[] {
  try {
    const raw = sessionStorage.getItem(KEY)
    if (!raw) return []
    const parsed = JSON.parse(raw) as PendingEntry[] | CustomerListItem[]
    // Migrate legacy bare CustomerListItem[] if present.
    if (Array.isArray(parsed) && parsed.length > 0 && !('customer' in parsed[0])) {
      const now = Date.now()
      return (parsed as CustomerListItem[]).map((customer) => ({ customer, at: now }))
    }
    return parsed as PendingEntry[]
  } catch {
    return []
  }
}

function writePendingEntries(entries: PendingEntry[]): void {
  sessionStorage.setItem(KEY, JSON.stringify(entries))
}

/** Live pending rows still within the visibility-lag TTL. */
export function readPending(): CustomerListItem[] {
  const now = Date.now()
  const fresh = readPendingEntries().filter((e) => now - e.at < PENDING_TTL_MS)
  if (fresh.length !== readPendingEntries().length) {
    writePendingEntries(fresh)
  }
  return fresh.map((e) => e.customer)
}

export function clearPending(id: string): void {
  writePendingEntries(readPendingEntries().filter((e) => e.customer.customerId !== id))
}

export function customerToListItem(c: CustomerResponse): CustomerListItem {
  return {
    customerId: c.customerId,
    name: c.name,
    email: c.email,
    points: c.points,
    level: c.level,
    enrolledAt: c.enrolledAt,
    generation: c.generation,
    status: c.status,
    runId: c.runId,
  }
}

/**
 * Whether a list item would match the effective visibility query.
 * Covers the chip-built queries and the same small clause set the mock understands.
 */
export function matchesVisibilityQuery(c: CustomerListItem, query: string): boolean {
  const q = query.trim()
  if (!q) return true

  for (const clause of q.split(' AND ').map((s) => s.trim()).filter(Boolean)) {
    if (clause.startsWith('RewardsLevel')) {
      if (!clause.includes(`'${c.level}'`)) return false
    } else if (clause.startsWith('RewardsActive')) {
      const wantActive = clause.includes('true')
      if (wantActive !== (c.status === 'active')) return false
    } else if (clause.startsWith('ExecutionStatus')) {
      // Legacy chip queries; soft-inactive uses RewardsActive instead.
      const want = c.status === 'deactivated' ? 'Canceled' : 'Running'
      if (!clause.includes(`'${want}'`)) return false
    } else if (clause.startsWith('CustomerName STARTS_WITH')) {
      // One clause per term, so this is one prefix against the name's tokens --
      // split by the same function that produced the term, rather than
      // substring-matched, which would show rows the server won't.
      const literal = clause.match(/'((?:[^'\\]|\\.)*)'/)?.[1] ?? ''
      const wanted = literal.replace(/\\(.)/g, '$1').toLowerCase()
      if (!nameTerms(c.name).some((t) => t.startsWith(wanted))) return false
    } else if (clause.startsWith('RewardsPoints >=')) {
      const n = Number(clause.split('>=')[1]?.trim() ?? NaN)
      if (!Number.isFinite(n) || c.points < n) return false
    } else {
      // Unknown clause — don't force the optimistic row into filtered views.
      return false
    }
  }
  return true
}

/**
 * Merge server items with pending optimistic rows that still match the filter.
 * Extras may briefly push the table past `limit`, rather than dropping a real
 * server row to make room.
 */
export function mergeWithPending(
  items: CustomerListItem[],
  pending: CustomerListItem[],
  query: string,
): CustomerListItem[] {
  const seen = new Set(items.map((i) => i.customerId))
  const extras = pending.filter(
    (p) => !seen.has(p.customerId) && matchesVisibilityQuery(p, query),
  )
  return [...extras, ...items]
}
