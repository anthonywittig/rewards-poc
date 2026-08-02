import { useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { listCustomers, temporalUiUrl } from '../api'
import { ErrorBanner } from '../components/ErrorBanner'
import { TierBadge } from '../components/TierBadge'
import { buildListQuery, formatDate, tierLabel } from '../format'
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

  const query = useMemo(
    () => buildListQuery({ tier, status, name }),
    [tier, status, name],
  )

  // Search as you type, one request per pause rather than one per keystroke.
  useEffect(() => {
    const t = window.setTimeout(() => setName(search), SEARCH_DEBOUNCE_MS)
    return () => window.clearTimeout(t)
  }, [search])

  // Tagging responses with their query keeps "does this belong to the filter on
  // screen?" a derived value, so rows cannot render under a query they did not
  // match.
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
      } catch (err) {
        if (cancelled) return
        setFailed({ query, err })
      }
    }

    void run()
    return () => {
      cancelled = true
    }
  }, [query])

  // The rows and chrome stay on the last response while the next one loads:
  // unmounting the notices shifts the table ~90px on every pause in typing, and
  // clearing the rows blanks the table on searches that change nothing. A
  // just-created customer may be missing here for a beat — that is the
  // visibility lag itself, worth seeing rather than papering over.
  const shown = data ?? (loading ? loaded?.res ?? null : null)

  const items = useMemo(() => {
    let rows = shown?.items ?? []
    if (shown?.complete && sortKey) {
      rows = [...rows].sort((a, b) => compare(a, b, sortKey, sortDir))
    }
    return rows
  }, [shown, sortKey, sortDir])

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

  function toggleSort(key: SortKey) {
    if (!shown?.complete || !key) return
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
      {shown && !shown.complete ? (
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
                enabled={!!shown?.complete}
                onClick={() => toggleSort('name')}
              />
              <th>Tier</th>
              <SortableTh
                label="Points"
                active={sortKey === 'points'}
                dir={sortDir}
                enabled={!!shown?.complete}
                onClick={() => toggleSort('points')}
              />
              <th>Status</th>
              <SortableTh
                label="Enrolled"
                active={sortKey === 'enrolledAt'}
                dir={sortDir}
                enabled={!!shown?.complete}
                onClick={() => toggleSort('enrolledAt')}
              />
              <th>Gen</th>
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
                <td>{c.generation}</td>
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
