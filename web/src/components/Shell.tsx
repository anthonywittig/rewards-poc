import { Link } from 'react-router-dom'
import { apiBase } from '../api'

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
        <div className="topbar-meta">
          API <code>{apiBase()}</code>
          <div>
            Temporal UI{' '}
            <a href="http://localhost:8080" target="_blank" rel="noreferrer">
              :8080
            </a>
          </div>
        </div>
      </header>
      {children}
    </div>
  )
}
