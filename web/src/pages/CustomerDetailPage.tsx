import { useEffect, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import {
  addPoints,
  deactivateCustomer,
  enrollCustomer,
  getAudit,
  getCustomer,
  newRequestId,
} from '../api'
import { AuditTimeline } from '../components/AuditTimeline'
import { ErrorBanner } from '../components/ErrorBanner'
import { ProgressBar } from '../components/ProgressBar'
import { TierBadge } from '../components/TierBadge'
import { formatDate, tierLabel } from '../format'
import type { AddPointsResponse, AuditResponse, CustomerResponse } from '../types'
import { ApiError } from '../types'

export function CustomerDetailPage() {
  const { id = '' } = useParams()
  const [customer, setCustomer] = useState<CustomerResponse | null>(null)
  const [audit, setAudit] = useState<AuditResponse | null>(null)
  const [error, setError] = useState<unknown>(null)
  const [auditError, setAuditError] = useState<unknown>(null)
  const [loading, setLoading] = useState(true)

  const [amount, setAmount] = useState('100')
  const [reason, setReason] = useState('purchase')
  const [pointsBusy, setPointsBusy] = useState(false)
  const [pointsError, setPointsError] = useState<unknown>(null)
  const [pointsOk, setPointsOk] = useState<AddPointsResponse | null>(null)

  const [confirmLeave, setConfirmLeave] = useState(false)
  const [leaveBusy, setLeaveBusy] = useState(false)
  const [leaveError, setLeaveError] = useState<unknown>(null)

  const [rejoinBusy, setRejoinBusy] = useState(false)
  const [rejoinError, setRejoinError] = useState<unknown>(null)

  useEffect(() => {
    if (!id) return
    let cancelled = false

    async function load(requestedId: string) {
      setLoading(true)
      setError(null)
      setAuditError(null)
      setCustomer(null)
      setAudit(null)
      setPointsOk(null)
      setPointsError(null)
      setConfirmLeave(false)
      try {
        const [c, a] = await Promise.all([
          getCustomer(requestedId),
          getAudit(requestedId).catch((err) => {
            if (!cancelled) setAuditError(err)
            return null
          }),
        ])
        if (cancelled) return
        setCustomer(c)
        setAudit(a)
      } catch (err) {
        if (cancelled) return
        setError(err)
        setCustomer(null)
      } finally {
        if (!cancelled) setLoading(false)
      }
    }

    void load(id)
    return () => {
      cancelled = true
    }
  }, [id])

  async function refresh() {
    if (!id) return
    const requestedId = id
    setLoading(true)
    setError(null)
    setAuditError(null)
    try {
      const [c, a] = await Promise.all([
        getCustomer(requestedId),
        getAudit(requestedId).catch((err) => {
          if (requestedId === id) setAuditError(err)
          return null
        }),
      ])
      if (requestedId !== id) return
      setCustomer(c)
      setAudit(a)
    } catch (err) {
      if (requestedId !== id) return
      setError(err)
      setCustomer(null)
    } finally {
      if (requestedId === id) setLoading(false)
    }
  }

  async function onAddPoints(e: React.FormEvent) {
    e.preventDefault()
    if (!customer || customer.status !== 'active') return
    const requestedId = customer.customerId
    setPointsBusy(true)
    setPointsError(null)
    setPointsOk(null)
    try {
      const res = await addPoints(requestedId, {
        amount: Number(amount),
        reason: reason.trim(),
        requestId: newRequestId(),
      })
      if (requestedId !== id) return
      setPointsOk(res)
      await refresh()
    } catch (err) {
      if (requestedId !== id) return
      setPointsError(err)
      // Handler rejections leave an audit row; validator ones do not.
      if (err instanceof ApiError && err.code === 'rejected') {
        try {
          const a = await getAudit(requestedId)
          if (requestedId === id) setAudit(a)
        } catch {
          /* ignore */
        }
      }
    } finally {
      if (requestedId === id) setPointsBusy(false)
    }
  }

  // Re-enrollment is the same POST /api/customers a new signup uses: the server
  // sees the ID is taken and inactive, and reactivates instead of starting.
  // Resending the name we already hold keeps this one click.
  async function onReactivate() {
    if (!customer) return
    const requestedId = customer.customerId
    setRejoinBusy(true)
    setRejoinError(null)
    try {
      await enrollCustomer({
        customerId: requestedId,
        name: customer.name,
      })
      if (requestedId !== id) return
      await refresh()
    } catch (err) {
      if (requestedId !== id) return
      setRejoinError(err)
    } finally {
      if (requestedId === id) setRejoinBusy(false)
    }
  }

  async function onDeactivate() {
    if (!customer) return
    const requestedId = customer.customerId
    setLeaveBusy(true)
    setLeaveError(null)
    try {
      await deactivateCustomer(requestedId)
      if (requestedId !== id) return
      setConfirmLeave(false)
      await refresh()
    } catch (err) {
      if (requestedId !== id) return
      setLeaveError(err)
    } finally {
      if (requestedId === id) setLeaveBusy(false)
    }
  }

  if (loading && !customer) {
    return (
      <>
        <Link className="back-link" to="/">
          ← Customers
        </Link>
        <p className="muted">Loading…</p>
      </>
    )
  }

  if (!customer) {
    return (
      <>
        <Link className="back-link" to="/">
          ← Customers
        </Link>
        <ErrorBanner error={error} />
      </>
    )
  }

  const active = customer.status === 'active'

  return (
    <>
      <Link className="back-link" to="/">
        ← Customers
      </Link>
      <ErrorBanner error={error} />

      <div className="detail-grid">
        <section className="detail-hero">
          <TierBadge level={customer.level} />
          <h1 className="name">{customer.name}</h1>
          <p className="customer-id">{customer.customerId}</p>

          <div className="points-block">
            <div className="points">
              {customer.points.toLocaleString()}
              <span>points</span>
            </div>
            <ProgressBar
              points={customer.points}
              nextTierAt={customer.nextTierAt}
              level={customer.level}
              tiers={customer.tiers}
            />
          </div>

          <div className="meta-row">
            <span>
              Status{' '}
              <strong className={`status-pill status-${customer.status}`}>
                {customer.status}
              </strong>
            </span>
            <span>
              Enrolled <strong>{formatDate(customer.enrolledAt)}</strong>
            </span>
            <span>
              Generation <strong>{customer.generation}</strong>
            </span>
            <span>
              Lifetime earns <strong>{customer.lifetimeEarnEvents}</strong>
            </span>
          </div>
        </section>

        <div className="side-panel">
          {active ? (
            <section className="panel">
              <h2>Add points</h2>
              <form onSubmit={onAddPoints}>
                <div className="field">
                  <label htmlFor="amount">Amount</label>
                  <input
                    id="amount"
                    type="number"
                    required
                    value={amount}
                    onChange={(e) => setAmount(e.target.value)}
                  />
                  <span className="hint">
                    Leave validation to the API — try -5 (validator) or a cap-busting
                    amount on capped customers (handler).
                  </span>
                </div>
                <div className="field" style={{ marginTop: '0.65rem' }}>
                  <label htmlFor="reason">Reason</label>
                  <input
                    id="reason"
                    required
                    value={reason}
                    onChange={(e) => setReason(e.target.value)}
                  />
                </div>
                <div className="form-actions">
                  <button className="btn btn-primary" type="submit" disabled={pointsBusy}>
                    {pointsBusy ? 'Applying…' : 'Add points'}
                  </button>
                </div>
              </form>
              <ErrorBanner error={pointsError} />
              {pointsOk ? (
                <p className="success">
                  Balance {pointsOk.balance.toLocaleString()} ·{' '}
                  {tierLabel(pointsOk.level)}
                </p>
              ) : null}
              <p className="hint" style={{ marginTop: '0.75rem' }}>
                Each click sends a fresh <code>requestId</code>. Validator rejections
                never appear in the audit log; handler (cap) rejections do.
              </p>
            </section>
          ) : (
            <section className="panel">
              <h2>Deactivated</h2>
              <p className="warn-copy">
                This customer has left the program. Points cannot be added.
                Re-enrolling restores their {customer.points.toLocaleString()}{' '}
                point balance — the workflow is still running, so nothing was lost.
              </p>
              <div className="form-actions">
                <button
                  type="button"
                  className="btn btn-primary"
                  disabled={rejoinBusy}
                  onClick={() => void onReactivate()}
                >
                  {rejoinBusy ? 'Re-enrolling…' : 'Re-enroll'}
                </button>
              </div>
              <ErrorBanner error={rejoinError} />
            </section>
          )}

          {active ? (
            <section className="panel">
              <h2>Leave program</h2>
              {!confirmLeave ? (
                <button
                  type="button"
                  className="btn btn-danger"
                  onClick={() => setConfirmLeave(true)}
                >
                  Deactivate
                </button>
              ) : (
                <>
                  <p className="warn-copy">
                    Soft-deactivate this customer. Their points are kept, and
                    re-enrolling later restores them.
                  </p>
                  <div className="form-actions">
                    <button
                      type="button"
                      className="btn btn-danger"
                      disabled={leaveBusy}
                      onClick={() => void onDeactivate()}
                    >
                      {leaveBusy ? 'Deactivating…' : 'Confirm deactivate'}
                    </button>
                    <button
                      type="button"
                      className="btn btn-ghost"
                      onClick={() => setConfirmLeave(false)}
                    >
                      Keep active
                    </button>
                  </div>
                </>
              )}
              <ErrorBanner error={leaveError} />
            </section>
          ) : null}
        </div>
      </div>

      <section className="panel" style={{ marginTop: '1.25rem' }}>
        <h2>Audit timeline</h2>
        <ErrorBanner error={auditError} />
        {audit ? <AuditTimeline audit={audit} /> : null}
      </section>
    </>
  )
}
