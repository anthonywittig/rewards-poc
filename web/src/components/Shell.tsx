import { Link } from 'react-router-dom'

// No Temporal UI link up here: the client holds no Temporal configuration.
// The links into the Temporal UI come pre-built in API responses — the list
// page's query link and the audit timeline's run dividers.
export function Shell({ children }: { children: React.ReactNode }) {
  return (
    <div className="app-shell">
      <header className="topbar">
        <div>
          <Link to="/" className="brand" style={{ textDecoration: 'none' }}>
            Rewards<span>.</span>
          </Link>
          <p className="brand-sub">
            Temporal is the ledger — points, tier, and history live in workflow executions, not a database.
          </p>
        </div>
      </header>
      {children}
    </div>
  )
}
