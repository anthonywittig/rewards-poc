import { useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { enrollCustomer, getCustomer } from '../api'
import { ErrorBanner } from '../components/ErrorBanner'
import { customerToListItem, rememberPending } from '../pending'

export function CreateCustomerPage() {
  const navigate = useNavigate()
  const [name, setName] = useState('')
  const [email, setEmail] = useState('')
  const [error, setError] = useState<unknown>(null)
  const [busy, setBusy] = useState(false)

  async function onSubmit(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      // No customerId: the server mints one and answers with it.
      const enrolled = await enrollCustomer({
        name: name.trim(),
        email: email.trim(),
      })
      // Detail is strongly consistent; list is not. Stash for optimistic list merge.
      try {
        const detail = await getCustomer(enrolled.customerId)
        rememberPending(customerToListItem(detail))
      } catch {
        rememberPending({
          customerId: enrolled.customerId,
          name: name.trim(),
          email: email.trim(),
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
            Creates a long-lived workflow under a server-assigned customer ID.
            After save you land on the detail page (Query/Describe), not the
            list — the visibility index lags writes.
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
            onChange={(e) => setName(e.target.value)}
            autoComplete="name"
          />
        </div>
        <div className="field">
          <label htmlFor="email">Email</label>
          <input
            id="email"
            type="email"
            required
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            autoComplete="email"
          />
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
