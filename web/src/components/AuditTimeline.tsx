import { Fragment } from 'react'
import { temporalRunUrl } from '../api'
import type { AuditEntry, AuditResponse } from '../types'
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
            const key = `${e.runId}-${e.eventId}-${e.kind}`

            if (e.kind === 'generation_rolled') {
              return (
                <RunStartDivider key={key} e={e} workflowId={audit.workflowId} />
              )
            }

            // Enrollment is two rows from one event: the membership fact, and
            // under it the same run-start divider every other generation gets.
            // Both name the WorkflowExecutionStarted event whose input is the
            // enrollment payload -- which is what makes the link worth having.
            if (e.kind === 'enrolled') {
              return (
                <Fragment key={key}>
                  <div className="timeline-item">
                    <div className="when">{formatWhen(e.at)}</div>
                    <div className="body">Enrolled</div>
                  </div>
                  <RunStartDivider e={e} workflowId={audit.workflowId} />
                </Fragment>
              )
            }

            const rejected = e.kind === 'points_rejected'
            return (
              <div
                key={key}
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

/** The run boundary, linked to that run's history in the Temporal UI. */
function RunStartDivider({ e, workflowId }: { e: AuditEntry; workflowId: string }) {
  return (
    <div className="timeline-item divider">
      <div className="when">{formatWhen(e.at)}</div>
      <div className="body">
        [debug]{' '}
        <a
          href={temporalRunUrl(workflowId, e.runId)}
          target="_blank"
          rel="noreferrer"
        >
          Generation {e.generation} —{' '}
          {e.kind === 'generation_rolled' ? 'continue-as-new workflow' : 'workflow'}{' '}
          started
        </a>
      </div>
    </div>
  )
}

function renderBody(e: AuditEntry): React.ReactNode {
  switch (e.kind) {
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
      return <>Deactivated — membership ended</>
    default:
      return <>{e.kind}</>
  }
}
