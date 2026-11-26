import type { SSHReport, SSHSession } from './ferry'

export type SessionFilter = 'all' | 'others' | 'local' | 'terminal' | 'non-ferry'

export interface SessionCounts {
  total: number
  users: number
  ips: number
  terminals: number
  pending: number
  localCount: number
  ferryCount: number
  otherDeviceCount: number
  sameIPOtherCount: number
  natSameIPCount: number
}

export interface OriginBadge {
  text: string
  type: 'success' | 'primary' | 'warning' | 'info'
  tip: string
}

export interface ActivityDisplay {
  summary: string
  raw: string
  hasFriendly: boolean
}

export interface AuthDisplay {
  text: string
  tip: string
}

export function isLocalSession(s: SSHSession): boolean {
  return !!s.self || s.origin === 'self' || s.origin === 'ferry-pool' || s.origin === 'local-proc'
}

export function isLocalFerrySession(s: SSHSession): boolean {
  return !!s.self || s.origin === 'self' || s.origin === 'ferry-pool'
}

export function isOtherDeviceSession(s: SSHSession): boolean {
  return !isLocalSession(s)
}

export function summarizeSessions(report?: SSHReport): SessionCounts | undefined {
  if (!report) return undefined
  const s = report.sessions ?? []
  const localCount = s.filter(isLocalSession).length
  const ferryCount = s.filter(isLocalFerrySession).length
  const otherDeviceCount = s.length - localCount
  const sameIPOtherCount = s.filter((x) => x.origin === 'same-ip').length
  const natSameIPCount = s.filter((x) => x.origin === 'nat-same-ip').length
  return {
    total: s.length,
    users: new Set(s.map((x) => x.user)).size,
    ips: new Set(s.filter((x) => x.from).map((x) => x.from)).size,
    terminals: s.filter((x) => x.kind === 'terminal').length,
    pending: report.pending ?? 0,
    localCount,
    ferryCount,
    otherDeviceCount,
    sameIPOtherCount,
    natSameIPCount,
  }
}

export function originBadge(s: SSHSession): OriginBadge | null {
  if (s.self || s.origin === 'self') {
    return {
      text: s.originLabel || '当前连接',
      type: 'success',
      tip: s.originDetail || '本机 Ferry 当前页面正在使用的连接（终端、文件、这个列表都经它）',
    }
  }
  switch (s.origin) {
    case 'ferry-pool':
      return {
        text: s.originLabel || '本机 Ferry（其他连接）',
        type: 'info',
        tip: s.originDetail || '本机 Ferry 连接池里维持的另一条 SSH 连接',
      }
    case 'local-proc':
      return {
        text: s.originLabel || (s.localProcess ? `本机 · ${s.localProcess}` : '本机其他进程'),
        type: 'primary',
        tip: s.originDetail || '本机上的其他客户端或进程建立的连接',
      }
    case 'same-ip':
      return {
        text: s.originLabel || '同 IP 其他设备',
        type: 'warning',
        tip: s.originDetail || '来源 IP 与本机相同，但本机未占用该源端口，通常是同一局域网 NAT / VPN 下的其他设备',
      }
    case 'nat-same-ip':
      return {
        text: s.originLabel || '同出口 IP',
        type: 'warning',
        tip: s.originDetail || '与本机来源 IP 相同，但经过了 NAT 端口转换，无法仅按源端口确认是否为本机',
      }
    case 'remote':
      return {
        text: s.originLabel || '其他 IP',
        type: 'info',
        tip: s.originDetail || '来自与当前本机不同的 IP 地址',
      }
    default:
      return null
  }
}

function friendlyCmdName(raw: string): { label: string; alias: boolean } {
  const s = raw.trim()
  if (!s) return { label: '', alias: false }
  if (s.includes('sftp-server') || s === 'internal-sftp') {
    return { label: '文件传输 (SFTP)', alias: true }
  }
  if (s.includes('ferry-agent-')) {
    return { label: 'Ferry 后台助手', alias: true }
  }
  const first = s.split(/\s+/)[0] ?? ''
  const base = first.replace(/^-/, '').split('/').pop() ?? ''
  if (/^(bash|zsh|sh|fish|dash|ash|ksh|csh|tcsh)$/.test(base) && (s === first || /\s-(l|-login)\b/.test(s))) {
    return { label: `交互终端 (${base})`, alias: true }
  }
  return { label: s, alias: false }
}

export function formatSessionActivity(s: SSHSession): ActivityDisplay {
  const rawParts = [...(s.cmds ?? [])]
  const rawLines = [...rawParts]
  if (s.forwards?.length) {
    rawLines.push(`端口转发: ${s.forwards.join(', ')}`)
  }
  const raw = rawLines.join('\n')

  const counts = new Map<string, number>()
  const order: string[] = []
  let hasFriendly = false
  for (const c of rawParts) {
    const f = friendlyCmdName(c)
    if (!f.label) continue
    if (f.alias) hasFriendly = true
    if (!counts.has(f.label)) order.push(f.label)
    counts.set(f.label, (counts.get(f.label) ?? 0) + 1)
  }
  const items = order.map((label) => {
    const n = counts.get(label) ?? 1
    return n > 1 ? `${label} ×${n}` : label
  })
  if (s.forwards?.length) {
    items.push(`端口转发 → ${s.forwards.join('、')}`)
  }
  return {
    summary: items.join(' · '),
    raw,
    hasFriendly,
  }
}

export function shortKeyFingerprint(fp: string): string {
  if (!fp) return ''
  if (fp.length <= 18) return fp
  return `${fp.slice(0, 18)}…`
}

export function formatSessionAuth(s: SSHSession): AuthDisplay | null {
  if (!s.authMethod) return null
  switch (s.authMethod) {
    case 'publickey': {
      if (s.authKeyComment) {
        return {
          text: `公钥 · ${s.authKeyComment}`,
          tip: s.authKey ? `公钥认证，备注：${s.authKeyComment}\n指纹：${s.authKey}` : `公钥认证，备注：${s.authKeyComment}`,
        }
      }
      if (s.authKey) {
        return {
          text: `公钥 · ${shortKeyFingerprint(s.authKey)}`,
          tip: `公钥认证\n指纹：${s.authKey}（authorized_keys 中未找到备注）`,
        }
      }
      return { text: '公钥登录', tip: '通过 SSH 公钥认证登录' }
    }
    case 'password':
      return { text: '密码登录', tip: '通过 SSH 密码认证登录' }
    case 'keyboard-interactive':
      return { text: '交互验证', tip: '通过 keyboard-interactive / PAM 认证登录' }
    default:
      return { text: s.authMethod, tip: `SSH 认证方式：${s.authMethod}` }
  }
}

export function matchesSessionQuery(s: SSHSession, query: string): boolean {
  const q = query.trim().toLowerCase()
  if (!q) return true
  const act = formatSessionActivity(s)
  const auth = formatSessionAuth(s)
  const hay = [
    String(s.pid),
    s.user,
    s.from ?? '',
    s.fromPort ? String(s.fromPort) : '',
    s.kind,
    ...(s.ttys ?? []),
    ...(s.cmds ?? []),
    ...(s.forwards ?? []),
    s.origin ?? '',
    s.originLabel ?? '',
    s.originDetail ?? '',
    s.localProcess ?? '',
    s.localPid ? String(s.localPid) : '',
    s.authMethod ?? '',
    s.authKey ?? '',
    s.authKeyComment ?? '',
    act.summary,
    auth?.text ?? '',
  ]
    .join(' ')
    .toLowerCase()
  return hay.includes(q)
}

export function filterSessions(sessions: SSHSession[], filter: SessionFilter, query: string): SSHSession[] {
  return sessions.filter((s) => {
    if (filter === 'others' && !isOtherDeviceSession(s)) return false
    if (filter === 'local' && !isLocalSession(s)) return false
    if (filter === 'terminal' && s.kind !== 'terminal') return false
    if (filter === 'non-ferry' && isLocalFerrySession(s)) return false
    return matchesSessionQuery(s, query)
  })
}

export function sessionFilterLabel(filter: SessionFilter): string {
  switch (filter) {
    case 'others':
      return '其他设备'
    case 'local':
      return '仅本机'
    case 'terminal':
      return '开着终端'
    case 'non-ferry':
      return '隐藏本机 Ferry'
    default:
      return '全部'
  }
}

const kindNames: Record<SSHSession['kind'], string> = {
  terminal: '终端',
  sftp: '传文件',
  ferry: 'Ferry',
  exec: '执行命令',
  idle: '空闲',
}

export function sessionShortDesc(s: SSHSession): string {
  const from = s.from ? `（来自 ${s.from}${s.fromPort ? `:${s.fromPort}` : ''}）` : ''
  const origin = s.originLabel ? ` [${s.originLabel}]` : ''
  const k = kindNames[s.kind] === '空闲' ? '连接' : kindNames[s.kind]
  return `${s.user}${from}${origin} 的${k}`
}

export function buildKickWarningLines(rows: SSHSession[]): string[] {
  const lines = rows.length > 1 ? rows.map((r) => `· ${sessionShortDesc(r)}`) : []
  const ferryPoolRows = rows.filter((r) => r.origin === 'ferry-pool')
  if (ferryPoolRows.length) {
    const labels = ferryPoolRows.map((r) => r.originLabel || '本机 Ferry 其他连接').join('、')
    lines.push('', `注意：包含本机 Ferry 正在使用的其他连接（${labels}），断开会中断对应连接页的终端、文件或后台操作。`)
  }
  const localProcRows = rows.filter((r) => r.origin === 'local-proc')
  if (localProcRows.length) {
    const procs = localProcRows
      .map((r) => (r.localProcess ? `${r.localProcess}${r.localPid ? ` (PID ${r.localPid})` : ''}` : r.originLabel || '本机进程'))
      .join('、')
    lines.push('', `提示：包含本机软件（${procs}）建立的 SSH 连接。`)
  }
  lines.push('', '里面正在运行的命令会被结束。对方可以重新连上：不想让它再连，要改密码、去掉它的公钥，或用防火墙限制来源。')
  return lines
}
