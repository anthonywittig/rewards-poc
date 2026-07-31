import type { CustomerListItem, CustomerResponse } from './types'

const KEY = 'rewards.pendingList'

/** Optimistically keep a just-created customer visible until ES catches up. */
export function rememberPending(customer: CustomerListItem): void {
  const next = [customer, ...readPending().filter((c) => c.customerId !== customer.customerId)]
  sessionStorage.setItem(KEY, JSON.stringify(next.slice(0, 20)))
}

export function readPending(): CustomerListItem[] {
  try {
    const raw = sessionStorage.getItem(KEY)
    if (!raw) return []
    return JSON.parse(raw) as CustomerListItem[]
  } catch {
    return []
  }
}

export function clearPending(id: string): void {
  sessionStorage.setItem(
    KEY,
    JSON.stringify(readPending().filter((c) => c.customerId !== id)),
  )
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

/** Merge server items with pending optimistic rows that still match the filter intent. */
export function mergeWithPending(
  items: CustomerListItem[],
  pending: CustomerListItem[],
  limit: number,
): CustomerListItem[] {
  const seen = new Set(items.map((i) => i.customerId))
  const extras = pending.filter((p) => !seen.has(p.customerId))
  return [...extras, ...items].slice(0, limit)
}
