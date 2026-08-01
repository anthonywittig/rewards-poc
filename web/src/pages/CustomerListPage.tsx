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

/** A response tagged with the query that produced it. */
interface Loaded {
  query: string
  res: CustomerListResponse
}

interface Failed {
  query: string
  err: unknown
}

export function CustomerListPage() {
  const [tier, setTier] = useState<string | null>(null)
  const [status, setStatus] = useState<'active' | 'deactivated' | 'any'>('active')
  const [search, setSearch] = useState('')
  const [name, setName] = useState('')
  const [sortKey, setSortKey] = useState<SortKey>(null)
  const [sortDir, setSortDir] = useState<SortDir>('desc')
  const [loaded, setLoaded] = useState<Loaded | null>(null)
  const [failed, setFailed] = useState<Failed | null>(null)
  const [pending, setPending] = useState(() => readPending())

  const query = useMemo(
    () => buildListQuery({ tier, status, name }),
    [tier, status, name],
  )

  // Search as you type, one request per pause rather than one per keystroke.
  useEffect(() => {
    const t = window.setTimeout(() => setName(search), SEARCH_DEBOUNCE_MS)
    return () => window.clearTimeout(t)
  }, [search])

  // Tagging responses with their query makes "does this belong to the filter on
  // screen?" a derived value, so rows still cannot render under a query they did
  // not match — but nothing has to be torn down and rebuilt to guarantee that.
  const data = loaded?.query === query ? loaded.res : null
  const error = failed?.query === query ? failed.err : null
  const loading = !data && !error

  useEffect(() => {
    let cancelled = false

    async function run() {
      try {
        const res = await listCustomers(query)
        if (cancelled) return
        setLoaded({ query, res })
        setFailed(null)
        const ids = new Set(res.items.map((i) => i.customerId))
        for (const p of readPending()) {
          if (ids.has(p.customerId)) clearPending(p.customerId)
        }
        setPending(readPending())
      } catch (err) {
        if (cancelled) return
        setFailed({ query, err })
      }
    }

    void run()

    // Visibility lag only has something to catch up to while an optimistic row
    // is still waiting to be indexed. Re-checking on every query change instead
    // doubled every search and made the table settle twice, half a second apart.
    const t = readPending().length
      ? window.setTimeout(() => {
          if (!cancelled) void run()
        }, 500)
      : undefined

    return () => {
      cancelled = true
      if (t !== undefined) window.clearTimeout(t)
    }
  }, [query])

  // Notices and the sort affordance stay on the last response while the next one
  // loads. Unmounting them shifted the table up ~90px and back on every pause in
  // typing; they describe the result set, not the rows, so holding them is safe.
  const chrome = data ?? (loading ? loaded?.res ?? null : null)

  const items = useMemo(() => {
    // Keep optimistic rows visible even when the list request failed.
    const serverItems = data?.items ?? []
    let rows = mergeWithPending(serverItems, pending, query)
    if (data?.complete && sortKey) {
      rows = [...rows].sort((a, b) => compare(a, b, sortKey, sortDir))
    }
    return rows
  }, [data, pending, sortKey, sortDir, query])

  // Blank rows standing in for the ones the previous query left on screen, so the
  // body holds its height instead of collapsing to a single “Loading…” row.
  const placeholders = loading
    ? Math.max((loaded?.res.items.length ?? 0) - items.length, items.length ? 0 : 1)
    : 0

  const incompleteNotice = useMemo(() => {
    if (!chrome || chrome.complete) return null
    const of =
      chrome.total < 0 ? 'many' : String(chrome.total)
    return `Showing ${chrome.items.length} of ${of} — filter to find additional results`
  }, [chrome])

  function toggleSort(key: SortKey) {
    if (!chrome?.complete || !key) return
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
      {chrome && !chrome.complete ? (
        <p className="hint" style={{ marginTop: '-0.5rem', marginBottom: '1rem' }}>
          Sorting is disabled until the result set fits in one page — sorting five
          arbitrary rows of a larger match would look authoritative and be wrong.
        </p>
      ) : null}

      <div className="table-wrap" aria-busy={loading}>
        <table>
          <thead>
            <tr>
              <SortableTh
                label="Name"
                active={sortKey === 'name'}
                dir={sortDir}
                enabled={!!chrome?.complete}
                onClick={() => toggleSort('name')}
              />
              <th>Tier</th>
              <SortableTh
                label="Points"
                active={sortKey === 'points'}
                dir={sortDir}
                enabled={!!chrome?.complete}
                onClick={() => toggleSort('points')}
              />
              <th>Status</th>
              <SortableTh
                label="Enrolled"
                active={sortKey === 'enrolledAt'}
                dir={sortDir}
                enabled={!!chrome?.complete}
                onClick={() => toggleSort('enrolledAt')}
              />
              <th>Gen</th>
            </tr>
          </thead>
          <tbody>
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
                  <div className="muted row-sub">
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
            {Array.from({ length: placeholders }, (_, i) => (
              <tr key={`placeholder-${i}`} className="row-placeholder">
                <td>
                  {i === 0 && items.length === 0 ? 'Loading…' : ' '}
                  {/* Mirrors the id · email line so the row is the same height. */}
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
