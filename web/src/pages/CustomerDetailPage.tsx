import { useEffect, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import {
  addPoints,
  deactivateCustomer,
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

  // Bumped by the action handlers to reload after a successful write. The rows
  // on screen stay put while the reload runs; only an id change blanks them.
  const [reloadTick, setReloadTick] = useState(0)
  const refresh = () => setReloadTick((t) => t + 1)

  // A new customer id is a new page: drop the previous customer's data and
  // form state before the load below repopulates it.
  useEffect(() => {
    setCustomer(null)
    setAudit(null)
    setPointsOk(null)
    setPointsError(null)
    setConfirmLeave(false)
  }, [id])

  useEffect(() => {
    if (!id) return
    let cancelled = false

    async function load() {
      setLoading(true)
      setError(null)
      setAuditError(null)
      try {
        const [c, a] = await Promise.all([
          getCustomer(id),
          getAudit(id).catch((err) => {
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

    void load()
    return () => {
      cancelled = true
    }
  }, [id, reloadTick])

  async function onAddPoints(e: React.FormEvent) {
    e.preventDefault()
    if (!customer || customer.status !== 'active') return
    setPointsBusy(true)
    setPointsError(null)
    setPointsOk(null)
    try {
      const res = await addPoints(customer.customerId, {
        amount: Number(amount),
        reason: reason.trim(),
        requestId: newRequestId(),
      })
      setPointsOk(res)
      refresh()
    } catch (err) {
      setPointsError(err)
      // Handler rejections leave an audit row worth showing; validator ones do
      // not, so skip the pointless reload.
      if (err instanceof ApiError && err.code === 'rejected') refresh()
    } finally {
      setPointsBusy(false)
    }
  }

  async function onDeactivate() {
    if (!customer) return
    setLeaveBusy(true)
    setLeaveError(null)
    try {
      await deactivateCustomer(customer.customerId)
      setConfirmLeave(false)
      refresh()
    } catch (err) {
      setLeaveError(err)
    } finally {
      setLeaveBusy(false)
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
        <div className="detail-main">
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
                prevTierAt={customer.prevTierAt}
                nextTierAt={customer.nextTierAt}
                level={customer.level}
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
                Run <strong>{customer.runNumber}</strong>
              </span>
              <span>
                Lifetime earns <strong>{customer.lifetimeEarnEvents}</strong>
              </span>
            </div>
          </section>

          <section className="panel">
            <h2>Audit timeline</h2>
            <ErrorBanner error={auditError} />
            {audit ? <AuditTimeline audit={audit} /> : null}
          </section>
        </div>

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
                This customer has left the program for good. Their workflow has
                completed, freezing the balance at{' '}
                {customer.points.toLocaleString()} points — deactivation is
                one-way, so points cannot be added and the membership cannot be
                restored.
              </p>
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
                    Deactivation is permanent: it completes this customer's
                    workflow and ends their membership. Their{' '}
                    {customer.points.toLocaleString()} points stay on record but
                    can never grow again.
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
    </>
  )
}
