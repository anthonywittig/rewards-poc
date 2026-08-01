import type { AuditResponse } from '../types'
import { formatWhen, tierLabel } from '../format'

export function AuditTimeline({ audit }: { audit: AuditResponse }) {
  const truncated =
    audit.truncated || audit.shownEarnEvents < audit.lifetimeEarnEvents

  return (
    <div>
      {truncated ? (
        <p className="notice timeline-notice">
          Showing {audit.shownEarnEvents} of {audit.lifetimeEarnEvents} point
          events. Earlier history has been deleted.
        </p>
      ) : null}

      {audit.entries.length === 0 ? (
        <p className="timeline-empty">No events yet — enrollment only.</p>
      ) : (
        <div className="timeline">
          {audit.entries.map((e) => {
            if (e.kind === 'generation_rolled') {
              return (
                <div
                  key={`${e.runId}-${e.eventId}-${e.kind}`}
                  className="timeline-item divider"
                >
                  <div className="when">{formatWhen(e.at)}</div>
                  <div className="body">
                    Generation {e.generation} — continue-as-new
                  </div>
                </div>
              )
            }

            const rejected = e.kind === 'points_rejected'
            return (
              <div
                key={`${e.runId}-${e.eventId}-${e.kind}`}
                className={`timeline-item${rejected ? ' rejected' : ''}`}
              >
                <div className="when">{formatWhen(e.at)}</div>
                <div className="body">{renderBody(e)}</div>
              </div>
            )
          })}
        </div>
      )}
    </div>
  )
}

function renderBody(e: AuditResponse['entries'][number]): React.ReactNode {
  switch (e.kind) {
    case 'enrolled':
      return <>Enrolled</>
    case 'points_added':
      return (
        <>
          +{e.amount?.toLocaleString()} <em>({e.reason})</em> →{' '}
          {e.balance?.toLocaleString()} · {tierLabel(e.level ?? '')}
        </>
      )
    case 'points_rejected':
      return (
        <>
          Rejected +{e.amount?.toLocaleString()} <em>({e.reason})</em>
          {e.failure ? (
            <>
              <br />
              <em>{e.failure}</em>
            </>
          ) : null}
        </>
      )
    case 'notification_sent':
      return <>Promoted to {tierLabel(e.notifiedLevel ?? '')} — notification sent</>
    case 'deactivated':
      // The balance comes from the workflow's completion payload, so it is
      // absent for a customer whose departure predates it and for the mock's
      // older rows. Say only what we know.
      return e.balance === undefined ? (
        <>Deactivated</>
      ) : (
        <>
          Deactivated — final balance {e.balance.toLocaleString()} ·{' '}
          {tierLabel(e.level ?? '')}
        </>
      )
    default:
      return <>{e.kind}</>
  }
}
