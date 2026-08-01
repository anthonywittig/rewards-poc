/** nextTierAt === 0 means no next tier (platinum / capped). Do not divide by it. */
export function ProgressBar({
  points,
  nextTierAt,
  tierFloor,
  level,
}: {
  points: number
  nextTierAt: number
  tierFloor: number
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

  // Both ends come from the server. Hardcoding "the rung below 1000 is 500"
  // stopped being true when the thresholds were versioned: a customer whose run
  // predates the change is on the original ladder and one enrolled today is 50
  // points below it, so the same nextTierAt no longer implies the same floor.
  const span = Math.max(nextTierAt - tierFloor, 1)
  const pct = Math.min(100, Math.max(0, ((points - tierFloor) / span) * 100))

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
