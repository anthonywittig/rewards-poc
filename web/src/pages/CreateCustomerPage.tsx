import { useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { enrollCustomer, getCustomer } from '../api'
import { ErrorBanner } from '../components/ErrorBanner'
import { slugifyId } from '../format'
import { customerToListItem, rememberPending } from '../pending'

export function CreateCustomerPage() {
  const navigate = useNavigate()
  const [name, setName] = useState('')
  const [customerId, setCustomerId] = useState('')
  const [idTouched, setIdTouched] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const [busy, setBusy] = useState(false)

  function onName(v: string) {
    setName(v)
    if (!idTouched) setCustomerId(slugifyId(v))
  }

  async function onSubmit(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      const id = customerId.trim() || slugifyId(name)
      const enrolled = await enrollCustomer({
        customerId: id,
        name: name.trim(),
      })
      // Detail is strongly consistent; list is not. Stash for optimistic list merge.
      try {
        const detail = await getCustomer(enrolled.customerId)
        rememberPending(customerToListItem(detail))
      } catch {
        rememberPending({
          customerId: enrolled.customerId,
          name: name.trim(),
          points: 0,
          level: 'basic',
          enrolledAt: new Date().toISOString(),
          generation: 0,
          status: 'active',
          runId: enrolled.runId,
        })
      }
      // Redirect to detail — never the list — so visibility lag cannot blank the UI.
      navigate(`/customers/${enrolled.customerId}`)
    } catch (err) {
      setError(err)
    } finally {
      setBusy(false)
    }
  }

  return (
    <>
      <Link className="back-link" to="/">
        ← Customers
      </Link>
      <div className="page-head">
        <div>
          <h1>Enroll customer</h1>
          <p>
            Creates a long-lived workflow. After save you land on the detail page
            (Query/Describe), not the list — the visibility index lags writes.
          </p>
        </div>
      </div>

      <ErrorBanner error={error} />

      <form className="form-panel" onSubmit={onSubmit}>
        <div className="field">
          <label htmlFor="name">Name</label>
          <input
            id="name"
            required
            value={name}
            onChange={(e) => onName(e.target.value)}
            autoComplete="name"
          />
        </div>
        <div className="field">
          <label htmlFor="cid">Customer ID</label>
          <input
            id="cid"
            required
            value={customerId}
            onChange={(e) => {
              setIdTouched(true)
              setCustomerId(e.target.value)
            }}
            pattern="[a-zA-Z0-9._-]+"
            title="Letters, digits, dots, underscores, hyphens"
          />
          <span className="hint">Becomes workflow ID customer-&lt;id&gt;</span>
        </div>
        <div className="form-actions">
          <button className="btn btn-primary" type="submit" disabled={busy}>
            {busy ? 'Enrolling…' : 'Enroll'}
          </button>
          <Link className="btn btn-ghost" to="/">
            Cancel
          </Link>
        </div>
      </form>
    </>
  )
}
