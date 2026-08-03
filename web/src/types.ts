/** Mirrors internal/httpapi/dto.go — frozen contract. Do not invent fields. */

export type CustomerStatus = 'active' | 'deactivated'

/** The stable machine-readable codes live in internal/httpapi/errors.go. */
export interface ApiErrorBody {
  code: string
  message: string
}

export class ApiError extends Error {
  readonly code: string

  constructor(body: ApiErrorBody) {
    super(body.message)
    this.name = 'ApiError'
    this.code = body.code
  }
}

export interface CustomerListItem {
  customerId: string
  name: string
  points: number
  level: string
  enrolledAt: string
  generation: number
  status: CustomerStatus
  runId: string
}

export interface CustomerListResponse {
  items: CustomerListItem[]
  limit: number
  total: number
  complete: boolean
  query?: string
}

/**
 * One rung of the tier ladder. `basic` is never in it — it is the floor, not a
 * rung, so the span from zero to the first rung is implied.
 */
export interface Tier {
  level: string
  minPoints: number
}

export interface CustomerResponse {
  customerId: string
  name: string
  points: number
  level: string
  nextTierAt: number
  /** Ascending by minPoints. Never null; see rewards.Ladder on the server. */
  tiers: Tier[]
  enrolledAt: string
  lifetimeEarnEvents: number
  generation: number
  status: CustomerStatus
  runId: string
}

export interface EnrollRequest {
  /**
   * Omitted by a signup from the UI — the server derives the ID from the name
   * and returns it. Callers that manage their own IDs (the seed) may send one.
   */
  customerId?: string
  name: string
}

export interface EnrollResponse {
  customerId: string
  workflowId: string
  runId: string
}

export interface AddPointsRequest {
  amount: number
  reason: string
  requestId?: string
}

export interface AddPointsResponse {
  balance: number
  level: string
}

export type AuditEntryKind =
  | 'enrolled'
  | 'points_added'
  | 'points_rejected'
  | 'generation_rolled'
  | 'deactivated'

export interface AuditEntry {
  kind: AuditEntryKind
  at: string
  generation: number
  runId: string
  eventId: number
  amount?: number
  reason?: string
  balance?: number
  level?: string
  failure?: string
  requestId?: string
}

export interface AuditResponse {
  customerId: string
  workflowId: string
  entries: AuditEntry[]
  truncated: boolean
  shownEarnEvents: number
  lifetimeEarnEvents: number
  oldestRunId?: string
  runsWalked: number
}
