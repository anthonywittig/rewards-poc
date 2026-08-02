import { tierLabel } from '../format'
import type { Tier } from '../types'

/**
 * Progress toward the next tier.
 *
 * Both ends come from the server: `nextTierAt` is the target, and `tiers` is
 * the ladder it was derived from, which is what supplies the *floor* — the rung
 * the customer is standing on. Deriving that floor from the thresholds directly
 * would put a second copy of the ladder in the client, and it would go stale in
 * silence: change a threshold and the bar keeps rendering, just at the wrong
 * width.
 *
 * `nextTierAt === 0` means no next tier (top of the ladder). Do not divide by it.
 */
export function ProgressBar({
  points,
  nextTierAt,
  level,
  tiers,
}: {
  points: number
  nextTierAt: number
  level: string
  tiers: Tier[]
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

  // The highest rung already reached, or 0 for basic — which is also what an
  // empty ladder degrades to, making the bar span the whole climb rather than
  // the current segment. Wrong, but bounded and monotonic, which is the most
  // useful thing to be when the ladder is missing.
  const prev = (tiers ?? []).reduce(
    (floor, t) => (t.minPoints <= points && t.minPoints > floor ? t.minPoints : floor),
    0,
  )
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
