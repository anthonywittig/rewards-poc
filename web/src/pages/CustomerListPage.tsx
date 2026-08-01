import { useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { listCustomers, temporalUiUrl } from '../api'
import { ErrorBanner } from '../components/ErrorBanner'
import { TierBadge } from '../components/TierBadge'
import { buildListQuery, formatDate } from '../format'
import { clearPending, mergeWithPending, readPending } from '../pending'
import type { CustomerListItem, CustomerListResponse } from '../types'

type SortKey = 'points' | 'name' | 'enrolledAt' | null
type SortDir = 'asc' | 'desc'

const TIERS = ['basic', 'gold', 'platinum'] as const

/** Long enough to coalesce a burst of keystrokes, short enough to feel live. */
const SEARCH_DEBOUNCE_MS = 250

export function CustomerListPage() {
  const [tier, setTier] = useState<string | null>(null)
  const [status, setStatus] = useState<'active' | 'deactivated' | 'any'>('active')
  const [search, setSearch] = useState('')
  const [name, setName] = useState('')
  const [sortKey, setSortKey] = useState<SortKey>(null)
  const [sortDir, setSortDir] = useState<SortDir>('desc')
  const [data, setData] = useState<CustomerListResponse | null>(null)
  const [pending, setPending] = useState(() => readPending())
  const [error, setError] = useState<unknown>(null)
  const [loading, setLoading] = useState(true)

  const query = useMemo(
    () => buildListQuery({ tier, status, name }),
    [tier, status, name],
  )

  // Search as you type, one request per pause rather than one per keystroke.
  useEffect(() => {
    const t = window.setTimeout(() => setName(search), SEARCH_DEBOUNCE_MS)
    return () => window.clearTimeout(t)
  }, [search])

  useEffect(() => {
    let cancelled = false
    // Drop the previous filter’s rows immediately so they cannot render under the new query.
    setData(null)
    setLoading(true)
    setError(null)

    async function run() {
      setLoading(true)
      setError(null)
      try {
        const res = await listCustomers(query)
        if (cancelled) return
        setData(res)
        const ids = new Set(res.items.map((i) => i.customerId))
        for (const p of readPending()) {
          if (ids.has(p.customerId)) clearPending(p.customerId)
        }
        setPending(readPending())
      } catch (err) {
        if (cancelled) return
        setError(err)
        setData(null)
      } finally {
        if (!cancelled) setLoading(false)
      }
    }

    void run()
    // Visibility lag: re-fetch once shortly after mount / query change.
    const t = window.setTimeout(() => {
      if (!cancelled) void run()
    }, 500)

    return () => {
      cancelled = true
      window.clearTimeout(t)
    }
  }, [query])

  const items = useMemo(() => {
    // Keep optimistic rows visible even when the list request failed.
    const serverItems = data?.items ?? []
    let rows = mergeWithPending(serverItems, pending, query)
    if (data?.complete && sortKey) {
      rows = [...rows].sort((a, b) => compare(a, b, sortKey, sortDir))
    }
    return rows
  }, [data, pending, sortKey, sortDir, query])

  const incompleteNotice = useMemo(() => {
    if (!data || data.complete) return null
    const of =
      data.total < 0 ? 'many' : String(data.total)
    return `Showing ${data.items.length} of ${of} — filter to find additional results`
  }, [data])

  function toggleSort(key: SortKey) {
    if (!data?.complete || !key) return
    if (sortKey === key) {
      setSortDir((d) => (d === 'asc' ? 'desc' : 'asc'))
    } else {
      setSortKey(key)
      setSortDir(key === 'name' ? 'asc' : 'desc')
    }
  }

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
              {t}
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

        <p className="hint">
          {query ? (
            <>
              Effective query: <code>{query}</code> — paste it into the{' '}
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
      {!data?.complete && data ? (
        <p className="hint" style={{ marginTop: '-0.5rem', marginBottom: '1rem' }}>
          Sorting is disabled until the result set fits in one page — sorting five
          arbitrary rows of a larger match would look authoritative and be wrong.
        </p>
      ) : null}

      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <SortableTh
                label="Name"
                active={sortKey === 'name'}
                dir={sortDir}
                enabled={!!data?.complete}
                onClick={() => toggleSort('name')}
              />
              <th>Tier</th>
              <SortableTh
                label="Points"
                active={sortKey === 'points'}
                dir={sortDir}
                enabled={!!data?.complete}
                onClick={() => toggleSort('points')}
              />
              <th>Status</th>
              <SortableTh
                label="Enrolled"
                active={sortKey === 'enrolledAt'}
                dir={sortDir}
                enabled={!!data?.complete}
                onClick={() => toggleSort('enrolledAt')}
              />
              <th>Gen</th>
            </tr>
          </thead>
          <tbody>
            {loading && !data && items.length === 0 ? (
              <tr>
                <td colSpan={6} className="muted">
                  Loading…
                </td>
              </tr>
            ) : null}
            {!loading && items.length === 0 ? (
              <tr>
                <td colSpan={6} className="muted">
                  No customers match this filter.
                  {name.trim()
                    ? ' Name search is a tokenized Text match — whole words only, so “lovelace” hits and “lovel” does not.'
                    : ''}
                </td>
              </tr>
            ) : null}
            {items.map((c) => (
              <tr key={c.customerId}>
                <td>
                  <Link to={`/customers/${c.customerId}`}>{c.name}</Link>
                  <div className="muted" style={{ fontSize: '0.78rem' }}>
                    {c.customerId} · {c.email}
                  </div>
                </td>
                <td>
                  <TierBadge level={c.level} />
                </td>
                <td>{c.points.toLocaleString()}</td>
                <td>
                  <span className={`status-pill status-${c.status}`}>{c.status}</span>
                </td>
                <td>{formatDate(c.enrolledAt)}</td>
                <td>{c.generation}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </>
  )
}

function SortableTh({
  label,
  active,
  dir,
  enabled,
  onClick,
}: {
  label: string
  active: boolean
  dir: SortDir
  enabled: boolean
  onClick: () => void
}) {
  return (
    <th
      className="sortable"
      aria-disabled={!enabled}
      onClick={onClick}
      title={enabled ? 'Sort' : 'Sorting only when the full result set fits one page'}
    >
      {label}
      {active ? (dir === 'asc' ? ' ↑' : ' ↓') : null}
    </th>
  )
}

function compare(
  a: CustomerListItem,
  b: CustomerListItem,
  key: Exclude<SortKey, null>,
  dir: SortDir,
): number {
  let cmp = 0
  if (key === 'points') cmp = a.points - b.points
  else if (key === 'name') cmp = a.name.localeCompare(b.name)
  else cmp = a.enrolledAt.localeCompare(b.enrolledAt)
  return dir === 'asc' ? cmp : -cmp
}
