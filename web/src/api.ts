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

// Default to the mock. Switch with VITE_API_BASE=http://localhost:8081 for the real API.
const API_BASE = (import.meta.env.VITE_API_BASE as string | undefined)?.replace(/\/$/, '')
  ?? 'http://localhost:8082'

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`${API_BASE}${path}`, {
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
      throw new ApiError(res.status, {
        code: 'internal',
        message: text || res.statusText || 'non-JSON response',
      })
    }
  }

  if (!res.ok) {
    const err = data as { error?: ApiErrorBody } | null
    if (err?.error?.code) {
      throw new ApiError(res.status, err.error)
    }
    throw new ApiError(res.status, {
      code: 'internal',
      message: text || res.statusText || 'request failed',
    })
  }

  return data as T
}

export function apiBase(): string {
  return API_BASE
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
