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
  status: CustomerStatus
  runNumber: number
  runId: string
}

export interface CustomerListResponse {
  items: CustomerListItem[]
  limit: number
  total: number
  complete: boolean
  query?: string
  /** The Temporal UI's workflow list, pre-filled with `query`. */
  queryUrl: string
}

export interface CustomerResponse {
  customerId: string
  name: string
  points: number
  level: string
  /**
   * The current segment of the tier climb: the rung the customer is standing
   * on (0 for basic) and the one they are climbing to (0 at the top). Derived
   * server-side from the same ladder as `level`.
   */
  prevTierAt: number
  nextTierAt: number
  enrolledAt: string
  status: CustomerStatus
  lifetimeEarnEvents: number
  runNumber: number
  runId: string
}

export interface EnrollRequest {
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
  | 'run_rolled'
  | 'deactivated'

export interface AuditEntry {
  kind: AuditEntryKind
  at: string
  runNumber: number
  runId: string
  eventId: number
  /** Deep link to this entry's run in the Temporal UI, built server-side. */
  historyUrl: string
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
