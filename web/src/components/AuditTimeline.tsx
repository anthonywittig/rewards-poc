import { temporalRunUrl } from '../api'
import type { AuditResponse } from '../types'
import { formatWhen, tierLabel } from '../format'

export function AuditTimeline({ audit }: { audit: AuditResponse }) {
  const truncated =
    audit.truncated || audit.shownEarnEvents < audit.lifetimeEarnEvents

  return (
    <div>
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
                    [debug]{' '}
                    <a
                      href={temporalRunUrl(audit.workflowId, e.runId)}
                      target="_blank"
                      rel="noreferrer"
                    >
                      Generation {e.generation} — continue-as-new workflow
                      started
                    </a>
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

      {/* Newest first, so the gap the crawl ran into is at the bottom of the
          list -- the notice sits where the missing events would have been. */}
      {truncated ? (
        <p className="notice timeline-notice">
          Showing {audit.shownEarnEvents} of {audit.lifetimeEarnEvents} point
          events. Earlier history has been deleted.
        </p>
      ) : null}
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
    case 'deactivated':
      return <>Deactivated — points kept</>
    case 'reactivated':
      return <>Re-enrolled — prior balance restored</>
    default:
      return <>{e.kind}</>
  }
}
