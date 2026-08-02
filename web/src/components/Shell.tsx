import { Link } from 'react-router-dom'
import { temporalUiUrl } from '../api'

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
          <div>
            Temporal UI{' '}
            <a href={temporalUiUrl()} target="_blank" rel="noreferrer">
              {temporalUiUrl()}
            </a>
          </div>
        </div>
      </header>
      {children}
    </div>
  )
}
