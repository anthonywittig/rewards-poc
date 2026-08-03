import { tierLabel } from '../format'

/**
 * Progress toward the next tier. Both ends of the segment come from the
 * server — `prevTierAt` is the rung the customer is standing on, `nextTierAt`
 * the target — so the client holds no copy of the tier thresholds.
 *
 * `nextTierAt === 0` means no next tier (top of the ladder). Do not divide by it.
 */
export function ProgressBar({
  points,
  prevTierAt,
  nextTierAt,
  level,
}: {
  points: number
  prevTierAt: number
  nextTierAt: number
  level: string
}) {
  if (nextTierAt <= 0) {
    return (
      <div className="progress">
        <div className="progress-track">
          <div className="progress-fill" style={{ width: '100%' }} />
        </div>
        <div className="progress-meta">
          <span>Top tier — {tierLabel(level)}</span>
          <span>no next threshold</span>
        </div>
      </div>
    )
  }

  const span = Math.max(nextTierAt - prevTierAt, 1)
  const pct = Math.min(100, Math.max(0, ((points - prevTierAt) / span) * 100))

  return (
    <div className="progress">
      <div className="progress-track">
        <div className="progress-fill" style={{ width: `${pct}%` }} />
      </div>
      <div className="progress-meta">
        <span>
          {points.toLocaleString()} → {nextTierAt.toLocaleString()}
        </span>
        <span>{Math.round(pct)}% to next tier</span>
      </div>
    </div>
  )
}
