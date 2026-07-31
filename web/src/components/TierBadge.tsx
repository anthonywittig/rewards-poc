import { tierLabel } from '../format'

export function TierBadge({ level }: { level: string }) {
  const cls =
    level === 'gold' ? 'tier-gold' : level === 'platinum' ? 'tier-platinum' : 'tier-basic'
  return <span className={`tier ${cls}`}>{tierLabel(level)}</span>
}
