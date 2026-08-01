/** Mirrors internal/httpapi/dto.go — frozen contract. Do not invent fields. */

export type CustomerStatus = 'active' | 'deactivated'

export type ErrorCode =
  | 'invalid_request'
  | 'not_found'
  | 'already_exists'
  | 'deactivated'
  | 'rollover_race'
  | 'rejected'
  | 'worker_unavailable'
  | 'internal'

export interface ApiErrorBody {
  code: ErrorCode | string
  message: string
}

export class ApiError extends Error {
  readonly code: string
  readonly status: number

  constructor(status: number, body: ApiErrorBody) {
    super(body.message)
    this.name = 'ApiError'
    this.code = body.code
    this.status = status
  }
}

export interface CustomerListItem {
  customerId: string
  name: string
  email: string
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

export interface CustomerResponse {
  customerId: string
  name: string
  email: string
  points: number
  level: string
  nextTierAt: number
  /** Balance that earned the current tier; 0 for basic. Server-supplied because
   *  the thresholds are versioned per workflow run — the client cannot infer the
   *  bottom of the rung from nextTierAt. */
  tierFloor: number
  enrolledAt: string
  lifetimeEarnEvents: number
  generation: number
  status: CustomerStatus
  runId: string
}

export interface EnrollRequest {
  customerId: string
  name: string
  email: string
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
  eventId: string
}

export type AuditEntryKind =
  | 'enrolled'
  | 'points_added'
  | 'points_rejected'
  | 'notification_sent'
  | 'generation_rolled'
  | 'deactivated'
  | 'reactivated'

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
  notifiedLevel?: string
}

export interface AuditResponse {
  customerId: string
  entries: AuditEntry[]
  truncated: boolean
  shownEarnEvents: number
  lifetimeEarnEvents: number
  oldestRunId?: string
  runsWalked: number
}
