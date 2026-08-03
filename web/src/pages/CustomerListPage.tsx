import { useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { listCustomers, temporalUiUrl } from '../api'
import { ErrorBanner } from '../components/ErrorBanner'
import { TierBadge } from '../components/TierBadge'
import { formatDate, tierLabel } from '../format'
import type { CustomerListResponse } from '../types'

const TIERS = ['basic', 'gold', 'platinum'] as const

/** Long enough to coalesce a burst of keystrokes, short enough to feel live. */
const SEARCH_DEBOUNCE_MS = 250

/** A response tagged with the filters that produced it. */
interface Loaded {
  key: string
  res: CustomerListResponse
}

interface Failed {
  key: string
  err: unknown
}

export function CustomerListPage() {
  const [tier, setTier] = useState<string | null>(null)
  const [status, setStatus] = useState<'active' | 'deactivated' | 'any'>('active')
  const [search, setSearch] = useState('')
  const [name, setName] = useState('')
  const [loaded, setLoaded] = useState<Loaded | null>(null)
  const [failed, setFailed] = useState<Failed | null>(null)

  // The filters travel to the API as plain params; the visibility query is
  // built there and comes back as `query` on the response. This key only has to
  // identify a filter combination, not mean anything.
  const filterKey = JSON.stringify({ tier, status, name })

  // Search as you type, one request per pause rather than one per keystroke.
  useEffect(() => {
    const t = window.setTimeout(() => setName(search), SEARCH_DEBOUNCE_MS)
    return () => window.clearTimeout(t)
  }, [search])

  // Tagging responses with their filters keeps "does this belong to the filter
  // on screen?" a derived value, so rows cannot render under a filter they did
  // not match.
  const data = loaded?.key === filterKey ? loaded.res : null
  const error = failed?.key === filterKey ? failed.err : null
  const loading = !data && !error

  useEffect(() => {
    let cancelled = false

    async function run() {
      try {
        const res = await listCustomers({ tier, status, name })
        if (cancelled) return
        setLoaded({ key: filterKey, res })
        setFailed(null)
      } catch (err) {
        if (cancelled) return
        setFailed({ key: filterKey, err })
      }
    }

    void run()
    return () => {
      cancelled = true
    }
    // filterKey is derived from these three, so tagging stays consistent.
  }, [tier, status, name])

  // The rows and chrome stay on the last response while the next one loads:
  // unmounting the notices shifts the table ~90px on every pause in typing, and
  // clearing the rows blanks the table on searches that change nothing. A
  // just-created customer may be missing here for a beat — that is the
  // visibility lag itself, worth seeing rather than papering over.
  const shown = data ?? (loading ? loaded?.res ?? null : null)

  // Rendered in the order Temporal returned them, which is unspecified — the
  // honest presentation of an index with no ORDER BY.
  const items = shown?.items ?? []

  // Whether to say "nothing matched". True when the rows are empty *because the
  // response was* — the notice stays on screen across a refetch rather than
  // being swapped for a loading row and back on every pause in typing.
  // `aria-busy` below is the loading signal; the row does not have to move to
  // carry one.
  const showEmpty = shown ? shown.items.length === 0 : !loading

  // Blank rows so the body holds its height instead of collapsing to a single
  // “Loading…” row. The empty-state row holds the body on its own, so this only
  // covers replacing rows with rows, and the first load with nothing on screen.
  const placeholders =
    loading && !showEmpty
      ? Math.max((loaded?.res.items.length ?? 0) - items.length, items.length ? 0 : 1)
      : 0

  const incompleteNotice = useMemo(() => {
    if (!shown || shown.complete) return null
    const of =
      shown.total < 0 ? 'many' : String(shown.total)
    return `Showing ${shown.items.length} of ${of} — filter to find additional results`
  }, [shown])

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Customers</h1>
          <p>
            Visibility store only — five rows, no pagination. Filtering is what this
            index is for; paste the same query into the Temporal UI to prove it.
          </p>
        </div>
        <Link className="btn btn-primary" to="/new">
          Enroll customer
        </Link>
      </div>

      <div className="filters">
        <div className="chip-row">
          <span className="label">Tier</span>
          <button
            type="button"
            className="chip"
            aria-pressed={tier === null}
            onClick={() => setTier(null)}
          >
            Any
          </button>
          {TIERS.map((t) => (
            <button
              key={t}
              type="button"
              className="chip"
              aria-pressed={tier === t}
              onClick={() => setTier(t)}
            >
              {tierLabel(t)}
            </button>
          ))}
        </div>

        <div className="chip-row">
          <span className="label">Status</span>
          {(
            [
              ['active', 'Active'],
              ['deactivated', 'Deactivated'],
              ['any', 'Any'],
            ] as const
          ).map(([value, label]) => (
            <button
              key={value}
              type="button"
              className="chip"
              aria-pressed={status === value}
              onClick={() => setStatus(value)}
            >
              {label}
            </button>
          ))}
        </div>

        <div className="chip-row">
          <label className="label" htmlFor="name-q">
            Name
          </label>
          <input
            id="name-q"
            className="search-input"
            type="search"
            value={search}
            placeholder="Search by name…"
            autoComplete="off"
            onChange={(e) => setSearch(e.target.value)}
          />
        </div>

        {/* The query is the response's echo of what the API built, so it
            labels the rows actually on screen — during a refetch it trails the
            chips by one response, exactly like the rows do. */}
        <p className="hint">
          {shown?.query ? (
            <>
              Effective query: <code>{shown.query}</code> — paste it into the{' '}
              <a href={temporalUiUrl()} target="_blank" rel="noreferrer">
                Temporal UI
              </a>
              .
            </>
          ) : (
            <>
              No filters — listing every customer. Filters become a visibility query
              you can paste into the{' '}
              <a href={temporalUiUrl()} target="_blank" rel="noreferrer">
                Temporal UI
              </a>
              .
            </>
          )}
        </p>
      </div>

      <ErrorBanner error={error} />

      {incompleteNotice ? <p className="notice">{incompleteNotice}</p> : null}

      <div className="table-wrap" aria-busy={loading}>
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th>Tier</th>
              <th>Points</th>
              <th>Status</th>
              <th>Enrolled</th>
              <th>Run</th>
            </tr>
          </thead>
          <tbody>
            {showEmpty ? (
              <tr>
                <td colSpan={6} className="muted">
                  No customers match this filter.
                  {name.trim()
                    ? ' Name search matches word prefixes, and every word has to match: “ada lov” finds Ada Lovelace, “ada turing” finds nobody.'
                    : ''}
                </td>
              </tr>
            ) : null}
            {items.map((c) => (
              <tr key={c.customerId}>
                <td>
                  <Link to={`/customers/${c.customerId}`}>{c.name}</Link>
                  <div className="muted row-sub">{c.customerId}</div>
                </td>
                <td>
                  <TierBadge level={c.level} />
                </td>
                <td>{c.points.toLocaleString()}</td>
                <td>
                  <span className={`status-pill status-${c.status}`}>{c.status}</span>
                </td>
                <td>{formatDate(c.enrolledAt)}</td>
                <td>{c.runNumber}</td>
              </tr>
            ))}
            {Array.from({ length: placeholders }, (_, i) => (
              <tr key={`placeholder-${i}`} className="row-placeholder">
                <td>
                  {i === 0 && items.length === 0 ? 'Loading…' : ' '}
                  {/* Mirrors the customer-id line so the row is the same height. */}
                  <div className="row-sub">&nbsp;</div>
                </td>
                <td />
                <td />
                <td />
                <td />
                <td />
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </>
  )
}
