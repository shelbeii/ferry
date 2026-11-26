import type { Firewall, FirewallAuth, FirewallItem, FirewallKind } from './ferry'

export interface FirewallOption {
  kind: FirewallKind
  name: string
  tagline: string
  desc: string
  manageable: boolean
  canEnable: boolean
  recommended: boolean
  pkgHint: string
  note?: string
}

/** 按服务器软件包管理器返回可安装的防火墙类型（推荐方案排在最前）。 */
export function firewallOptions(pkgMgr?: string): FirewallOption[] {
  const isApt = pkgMgr === 'apt-get'
  const isRpm = pkgMgr === 'dnf' || pkgMgr === 'yum'
  const list: FirewallOption[] = [
    {
      kind: 'firewalld',
      name: 'firewalld',
      tagline: '动态区域防火墙',
      desc: 'RHEL / CentOS / Rocky / Alma 等系统默认方案，支持在 Ferry 里直接放行、关闭端口与防锁死开关。',
      manageable: true,
      canEnable: true,
      recommended: isRpm || !pkgMgr,
      pkgHint: '软件包：firewalld',
    },
    {
      kind: 'ufw',
      name: 'ufw',
      tagline: '简洁主机防火墙',
      desc: 'Ubuntu / Debian 默认方案，规则直观易用，支持在 Ferry 里直接放行、关闭端口与防锁死开关。',
      manageable: true,
      canEnable: true,
      recommended: isApt,
      pkgHint: '软件包：ufw',
      note: isRpm ? 'RPM 系服务器通常需先启用 EPEL 软件源才能安装 ufw。' : undefined,
    },
    {
      kind: 'nftables',
      name: 'nftables',
      tagline: '现代内核防火墙',
      desc: 'Linux 内核新一代高性能防火墙服务（/etc/nftables.conf）；安装后在终端管理规则，Ferry 查看拦截状态。',
      manageable: false,
      canEnable: false,
      recommended: false,
      pkgHint: '软件包：nftables',
    },
    {
      kind: 'iptables',
      name: 'iptables',
      tagline: '传统规则持久化服务',
      desc: '安装系统 iptables 持久化服务，适合已有 iptables 脚本的环境；规则在终端管理，Ferry 查看拦截状态。',
      manageable: false,
      canEnable: false,
      recommended: false,
      pkgHint: isApt ? '软件包：iptables-persistent' : '软件包：iptables-services',
    },
  ]
  return list.sort((a, b) => Number(b.recommended) - Number(a.recommended))
}

/** 默认选中的防火墙类型：DEB 系优先 ufw，RPM 系优先 firewalld。 */
export function defaultFirewallKind(pkgMgr?: string): FirewallKind {
  return pkgMgr === 'apt-get' ? 'ufw' : 'firewalld'
}

/** 可通过 Ferry 卸载的防火墙列表。 */
export function uninstallableFirewalls(fw?: Firewall): FirewallItem[] {
  return (fw?.installed ?? []).filter((item) => !!item.uninstall)
}

/** 会与安装新防火墙产生冲突的已安装防火墙。 */
export function blockingInstalledFirewall(fw?: Firewall): FirewallItem | undefined {
  return (fw?.installed ?? []).find((item) => item.active || item.kind === 'firewalld' || item.kind === 'ufw')
}

/** 当前是否允许通过 Ferry 安装防火墙。 */
export function canInstallFirewall(fw?: Firewall): boolean {
  return !!fw?.pkgMgr && !blockingInstalledFirewall(fw)
}

export interface AuthMethodMeta {
  method: FirewallAuth['method']
  badge: string
  label: string
  placeholder: string
  description: string
  isChallenge: boolean
}

/** 卸载认证弹窗的展示文案与模式。 */
export function authMethodMeta(auth: FirewallAuth | undefined, usePasswordFallback: boolean): AuthMethodMeta {
  const who = auth?.user && auth?.host ? `${auth.user}@${auth.host}:${auth.port || 22}` : '当前服务器'
  const effective: FirewallAuth['method'] =
    usePasswordFallback && auth?.canPasswordFallback ? 'ssh-password' : (auth?.method ?? 'ssh-key-challenge')
  switch (effective) {
    case 'sudo-password':
      return {
        method: 'sudo-password',
        badge: 'sudo 密码认证',
        label: '输入 sudo 密码',
        placeholder: `输入 ${who} 的 sudo 密码`,
        description: '本次输入将直接向服务器 sudo 实时校验，不使用已保存的凭据库或会话缓存密码。',
        isChallenge: false,
      }
    case 'ssh-password':
      return {
        method: 'ssh-password',
        badge: 'SSH 登录密码认证',
        label: '输入 SSH 登录密码',
        placeholder: `输入 ${who} 的登录密码`,
        description: '本次输入将建立全新 SSH 握手向服务器实时校验，不使用已保存的凭据库或会话缓存密码。',
        isChallenge: false,
      }
    case 'key-passphrase':
      return {
        method: 'key-passphrase',
        badge: 'SSH 私钥密码认证',
        label: '输入私钥密码',
        placeholder: '输入 SSH 私钥解密密码',
        description: `${auth?.prompt || '请输入 SSH 私钥密码'}。校验时会实时解密私钥并重新认证服务器。`,
        isChallenge: false,
      }
    default:
      return {
        method: 'ssh-key-challenge',
        badge: 'SSH 密钥 + 一次性认证码',
        label: '输入上方 6 位认证码',
        placeholder: '输入服务端签发的 6 位认证码',
        description: `此服务器使用免密 SSH 密钥登录（${who}）。提交时将重新校验当前 SSH 密钥权限并核销服务端一次性认证码。`,
        isChallenge: true,
      }
  }
}

export function firewallKindLabel(kind: string): string {
  switch (kind) {
    case 'firewalld':
      return 'firewalld'
    case 'ufw':
      return 'ufw'
    case 'nftables':
      return 'nftables'
    case 'iptables':
      return 'iptables 持久化服务'
    default:
      return kind
  }
}
