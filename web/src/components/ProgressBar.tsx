/** nextTierAt === 0 means no next tier (platinum / capped). Do not divide by it. */
export function ProgressBar({
  points,
  nextTierAt,
  level,
}: {
  points: number
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
          <span>Top tier — {level}</span>
          <span>no next threshold</span>
        </div>
      </div>
    )
  }

  const prev =
    nextTierAt === 500 ? 0 : nextTierAt === 1000 ? 500 : 0
  const span = Math.max(nextTierAt - prev, 1)
  const pct = Math.min(100, Math.max(0, ((points - prev) / span) * 100))

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
