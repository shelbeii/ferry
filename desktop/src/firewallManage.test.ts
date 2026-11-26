import { describe, expect, it } from 'vitest'
import { authMethodMeta, blockingInstalledFirewall, canInstallFirewall, defaultFirewallKind, firewallOptions, uninstallableFirewalls } from './firewallManage'
import type { Firewall } from './ferry'

describe('防火墙安装与卸载辅助逻辑', () => {
  it('按软件包管理器排序并推荐合适的防火墙类型', () => {
    const rpm = firewallOptions('dnf')
    expect(rpm[0].kind).toBe('firewalld')
    expect(rpm[0].recommended).toBe(true)
    expect(rpm.find((o) => o.kind === 'ufw')?.note).toContain('EPEL')
    expect(rpm.find((o) => o.kind === 'iptables')?.pkgHint).toContain('iptables-services')
    expect(defaultFirewallKind('dnf')).toBe('firewalld')

    const deb = firewallOptions('apt-get')
    expect(deb[0].kind).toBe('ufw')
    expect(deb[0].recommended).toBe(true)
    expect(deb.find((o) => o.kind === 'iptables')?.pkgHint).toContain('iptables-persistent')
    expect(defaultFirewallKind('apt-get')).toBe('ufw')
  })

  it('判断安装可用性与冲突状态', () => {
    const emptyRpm: Firewall = { active: false, manage: false, pkgMgr: 'dnf' }
    expect(canInstallFirewall(emptyRpm)).toBe(true)
    expect(uninstallableFirewalls(emptyRpm)).toEqual([])

    const noPkg: Firewall = { active: false, manage: false }
    expect(canInstallFirewall(noPkg)).toBe(false)

    const hasFwd: Firewall = {
      kind: 'firewalld',
      active: false,
      manage: false,
      pkgMgr: 'dnf',
      installed: [{ kind: 'firewalld', active: false, uninstall: true }],
    }
    expect(blockingInstalledFirewall(hasFwd)?.kind).toBe('firewalld')
    expect(canInstallFirewall(hasFwd)).toBe(false)
    expect(uninstallableFirewalls(hasFwd)).toHaveLength(1)

    // 仅安装了未启用的 nftables：既允许卸载，也允许继续安装 firewalld / ufw
    const inactiveNft: Firewall = {
      kind: 'nftables',
      active: false,
      manage: false,
      pkgMgr: 'apt-get',
      installed: [{ kind: 'nftables', active: false, uninstall: true }],
    }
    expect(canInstallFirewall(inactiveNft)).toBe(true)
    expect(uninstallableFirewalls(inactiveNft)).toHaveLength(1)
  })

  it('按认证方式生成正确的卸载认证文案并支持免密密钥切换密码认证', () => {
    const sudoMeta = authMethodMeta({ method: 'sudo-password', prompt: 'sudo', user: 'suidao3', host: '41.68.221.85', port: 22 }, false)
    expect(sudoMeta.method).toBe('sudo-password')
    expect(sudoMeta.isChallenge).toBe(false)
    expect(sudoMeta.badge).toBe('sudo 密码认证')

    const keyAuth = { method: 'ssh-key-challenge' as const, prompt: 'key', user: 'root', host: '10.0.0.1', port: 22, canPasswordFallback: true }
    expect(authMethodMeta(keyAuth, false).isChallenge).toBe(true)
    const fallback = authMethodMeta(keyAuth, true)
    expect(fallback.method).toBe('ssh-password')
    expect(fallback.isChallenge).toBe(false)

    // 不允许降级/切换非 fallback 目标
    const sshPw = { method: 'ssh-password' as const, prompt: 'pw', user: 'root', host: '10.0.0.1', port: 22 }
    expect(authMethodMeta(sshPw, true).method).toBe('ssh-password')
  })
})
