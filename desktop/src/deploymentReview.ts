import type { Inspect, ReleaseInfo } from './ferry'

/** 与配置的应用选择规则一致：显式选择不允许的应用时阻止部署，不静默跳过。 */
export function deploymentTargets(info: Inspect, targets: string[], selected: string[]) {
  return targets.flatMap((name) => {
    const target = info.targets.find((t) => t.name === name)
    if (!target) return []
    const wanted = selected.length ? selected : target.apps
    return [{
      target,
      apps: info.apps.filter((a) => wanted.includes(a.name) && target.apps.includes(a.name)).map((a) => a.name),
      unavailable: selected.filter((a) => !target.apps.includes(a) || !info.apps.some((item) => item.name === a)),
    }]
  })
}

/** 默认回滚只会取当前版本之前、没有失败标记的版本，与 agent.previousGood 一致。 */
export function previousRelease(releases: ReleaseInfo[]) {
  const current = releases.find((r) => r.current)?.id
  return releases.filter((r) => !r.failed && !r.current && (!current || r.id < current)).sort((a, b) => b.id.localeCompare(a.id))[0]
}
