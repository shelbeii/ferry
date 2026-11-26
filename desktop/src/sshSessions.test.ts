import { describe, expect, it } from 'vitest'
import type { SSHReport, SSHSession } from './ferry'
import {
  buildKickWarningLines,
  filterSessions,
  formatSessionActivity,
  formatSessionAuth,
  originBadge,
  summarizeSessions,
} from './sshSessions'

function mk(overrides: Partial<SSHSession>): SSHSession {
  return {
    pid: 100,
    user: 'root',
    from: '172.21.106.3',
    fromPort: 58338,
    since: 1000,
    idle: -1,
    kind: 'ferry',
    kick: true,
    ...overrides,
  }
}

describe('sshSessions helpers', () => {
  it('summarizes local vs other-device counts accurately', () => {
    const report: SSHReport = {
      priv: 'root',
      me: 'root',
      now: 2000,
      selfFrom: '172.21.106.3',
      sessions: [
        mk({ pid: 1, self: true, kick: false, origin: 'self', originLabel: '当前连接' }),
        mk({ pid: 2, origin: 'ferry-pool', originLabel: '本机 Ferry（独立连接：172.22.18.28）' }),
        mk({ pid: 3, origin: 'local-proc', originLabel: '本机 · goland64.exe', kind: 'terminal' }),
        mk({ pid: 4, origin: 'same-ip', originLabel: '同 IP 其他设备', kind: 'terminal' }),
        mk({ pid: 5, from: '10.0.0.9', origin: 'remote', originLabel: '其他 IP', kind: 'exec' }),
      ],
    }
    expect(summarizeSessions(report)).toEqual({
      total: 5,
      users: 1,
      ips: 2,
      terminals: 2,
      pending: 0,
      localCount: 3,
      ferryCount: 2,
      otherDeviceCount: 2,
      sameIPOtherCount: 1,
      natSameIPCount: 0,
    })
  })

  it('formats friendly activity labels while preserving raw commands and forwards', () => {
    const ferry = mk({
      cmds: [
        '/usr/libexec/openssh/sftp-server -l INFO -f AUTH',
        '/usr/local/lib/ferry/ferry-agent-abc123 serve',
      ],
    })
    const act1 = formatSessionActivity(ferry)
    expect(act1.summary).toBe('文件传输 (SFTP) · Ferry 后台助手')
    expect(act1.hasFriendly).toBe(true)
    expect(act1.raw).toContain('sftp-server')
    expect(act1.raw).toContain('ferry-agent-abc123')

    const term = mk({
      kind: 'terminal',
      cmds: ['-bash', '/usr/libexec/openssh/sftp-server -l INFO -f AUTH'],
      forwards: ['127.0.0.1:3306'],
    })
    const act2 = formatSessionActivity(term)
    expect(act2.summary).toBe('交互终端 (bash) · 文件传输 (SFTP) · 端口转发 → 127.0.0.1:3306')
    expect(act2.raw).toContain('127.0.0.1:3306')

    const cmd = mk({ kind: 'exec', cmds: ['sleep 121'] })
    const act3 = formatSessionActivity(cmd)
    expect(act3.summary).toBe('sleep 121')
    expect(act3.hasFriendly).toBe(false)
  })

  it('formats auth method and key comment cleanly', () => {
    const pubWithComment = formatSessionAuth(
      mk({ authMethod: 'publickey', authKey: 'SHA256:abcdefgh1234567890xyz', authKeyComment: 'iron@workstation' }),
    )
    expect(pubWithComment?.text).toBe('公钥 · iron@workstation')
    expect(pubWithComment?.tip).toContain('SHA256:abcdefgh1234567890xyz')

    const pubNoComment = formatSessionAuth(
      mk({ authMethod: 'publickey', authKey: 'SHA256:abcdefgh1234567890xyz' }),
    )
    expect(pubNoComment?.text).toBe('公钥 · SHA256:abcdefgh123…')

    expect(formatSessionAuth(mk({ authMethod: 'password' }))?.text).toBe('密码登录')
    expect(formatSessionAuth(mk({ authMethod: 'keyboard-interactive' }))?.text).toBe('交互验证')
    expect(formatSessionAuth(mk({}))).toBeNull()
  })

  it('filters by source category and matches search across origin, auth comment, and friendly text', () => {
    const list = [
      mk({ pid: 1, self: true, kick: false, origin: 'self', originLabel: '当前连接' }),
      mk({ pid: 2, origin: 'ferry-pool', originLabel: '本机 Ferry（独立连接：172.22.18.28）' }),
      mk({
        pid: 3,
        origin: 'local-proc',
        originLabel: '本机 · Xshell.exe',
        localProcess: 'Xshell.exe',
        kind: 'terminal',
        authMethod: 'publickey',
        authKeyComment: 'zhangsan@MacBook',
      }),
      mk({ pid: 4, origin: 'same-ip', originLabel: '同 IP 其他设备', kind: 'terminal' }),
    ]
    expect(filterSessions(list, 'others', '').map((x) => x.pid)).toEqual([4])
    expect(filterSessions(list, 'local', '').map((x) => x.pid)).toEqual([1, 2, 3])
    expect(filterSessions(list, 'non-ferry', '').map((x) => x.pid)).toEqual([3, 4])
    expect(filterSessions(list, 'terminal', '').map((x) => x.pid)).toEqual([3, 4])
    expect(filterSessions(list, 'all', 'zhangsan').map((x) => x.pid)).toEqual([3])
    expect(filterSessions(list, 'all', 'xshell').map((x) => x.pid)).toEqual([3])
    expect(filterSessions(list, 'all', '独立连接').map((x) => x.pid)).toEqual([2])
  })

  it('returns clear badges and safe kick warnings for ferry-pool and local-proc', () => {
    const poolRow = mk({ pid: 2, origin: 'ferry-pool', originLabel: '本机 Ferry（独立连接：172.22.18.28）' })
    const localRow = mk({ pid: 3, origin: 'local-proc', originLabel: '本机 · goland64.exe', localProcess: 'goland64.exe', localPid: 8840 })
    const sameIpRow = mk({ pid: 4, origin: 'same-ip', originLabel: '同 IP 其他设备' })

    expect(originBadge(poolRow)?.type).toBe('info')
    expect(originBadge(localRow)?.type).toBe('primary')
    expect(originBadge(sameIpRow)?.type).toBe('warning')

    const lines = buildKickWarningLines([poolRow, localRow])
    const joined = lines.join('\n')
    expect(joined).toContain('本机 Ferry 正在使用的其他连接')
    expect(joined).toContain('goland64.exe (PID 8840)')
  })
})
