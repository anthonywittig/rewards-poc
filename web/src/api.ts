import type {
  AddPointsRequest,
  AddPointsResponse,
  ApiErrorBody,
  AuditResponse,
  CustomerListResponse,
  CustomerResponse,
  EnrollRequest,
  EnrollResponse,
} from './types'
import { ApiError } from './types'

// Same-origin: Vite proxies /api to the Go API, which sends no CORS headers.
async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    ...init,
    headers: {
      Accept: 'application/json',
      ...(init?.body ? { 'Content-Type': 'application/json' } : {}),
      ...init?.headers,
    },
  })

  if (res.status === 204) {
    return undefined as T
  }

  const text = await res.text()
  let data: unknown = null
  if (text) {
    try {
      data = JSON.parse(text)
    } catch {
      throw new ApiError({
        code: 'internal',
        message: text || res.statusText || 'non-JSON response',
      })
    }
  }

  if (!res.ok) {
    const err = data as { error?: ApiErrorBody } | null
    if (err?.error?.code) {
      throw new ApiError(err.error)
    }
    throw new ApiError({
      code: 'internal',
      message: text || res.statusText || 'request failed',
    })
  }

  return data as T
}

// The web service sets VITE_TEMPORAL_UI_URL from TEMPORAL_UI_PORT, so a second
// stack's links do not point at the first stack's UI.
export function temporalUiUrl(): string {
  const fromEnv = import.meta.env.VITE_TEMPORAL_UI_URL as string | undefined
  return fromEnv?.replace(/\/$/, '') || 'http://localhost:8080'
}

export function listCustomers(query?: string): Promise<CustomerListResponse> {
  const q = query?.trim()
  const qs = q ? `?q=${encodeURIComponent(q)}` : ''
  return request<CustomerListResponse>(`/api/customers${qs}`)
}

export function getCustomer(id: string): Promise<CustomerResponse> {
  return request<CustomerResponse>(`/api/customers/${encodeURIComponent(id)}`)
}

export function enrollCustomer(body: EnrollRequest): Promise<EnrollResponse> {
  return request<EnrollResponse>('/api/customers', {
    method: 'POST',
    body: JSON.stringify(body),
  })
}

export function addPoints(id: string, body: AddPointsRequest): Promise<AddPointsResponse> {
  return request<AddPointsResponse>(`/api/customers/${encodeURIComponent(id)}/points`, {
    method: 'POST',
    body: JSON.stringify(body),
  })
}

export function deactivateCustomer(id: string): Promise<void> {
  return request<void>(`/api/customers/${encodeURIComponent(id)}`, {
    method: 'DELETE',
  })
}

export function getAudit(id: string): Promise<AuditResponse> {
  return request<AuditResponse>(`/api/customers/${encodeURIComponent(id)}/audit`)
}

/** Fresh UUID per click — Temporal UpdateID / idempotency key. */
export function newRequestId(): string {
  return crypto.randomUUID()
}
