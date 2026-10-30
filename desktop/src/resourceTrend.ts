export interface ResourcePoint { at: number; value: number | null }

/** Real observations only. Missing observations and long sampling gaps break the line. */
export function trendSegments(points: ResourcePoint[], gapMs: number): string[] {
  if (points.length < 2) return []
  const start = points[0].at
  const duration = points[points.length - 1].at - start
  if (duration <= 0) return []
  const segments: string[] = []
  let current: string[] = []
  let previous = start
  const flush = () => { if (current.length > 1) segments.push(current.join(' ')); current = [] }
  for (const p of points) {
    if (p.value == null || !Number.isFinite(p.value) || p.at - previous > gapMs) flush()
    if (p.value != null && Number.isFinite(p.value)) current.push(`${((p.at - start) / duration * 300).toFixed(2)},${(64 - Math.min(100, Math.max(0, p.value)) / 100 * 56).toFixed(2)}`)
    previous = p.at
  }
  flush()
  return segments
}
