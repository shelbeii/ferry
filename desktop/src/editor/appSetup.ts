import type { Document } from 'yaml'
import { append, hasAlias, keysAt, listAt, nodeAt, scalarAt, setList, setScalar } from './configDoc'

export type AppType = 'java' | 'binary' | 'static' | 'python' | 'node' | 'dotnet'
export interface DeploymentRequest { apps: string[]; targets: string[] }
export interface RuntimeProbe {
  javas?: { path: string; version: string; major: number }[] | null
  nginxInstances?: { name: string; unit: string; version?: string; hosting?: { system?: boolean; sites: string } }[] | null
  checks: { level: string; text: string }[]
}
export interface AppSetup {
  name: string
  type: AppType
  path: string
  targets: string[]
  build: string
  buildCommand: string
  buildWorkdir: string
  user: string
  javaSource: 'ferry' | 'server'
  javaMajor: number
  javaPaths: Record<string, string>
  jvmArgs: string
  args: string
  exec: string
  env: string
  port: string
  serve: 'installed' | 'standalone' | 'files'
  instances: Record<string, string>
  domains: string
  spa: boolean
  proxies: { path: string; to: string }[]
}

export const lines = (text: string) => text.split(/\r?\n/).map((s) => s.trim()).filter(Boolean)
export const appNames = (doc: Document): string[] => (doc.toJS()?.apps ?? []).map((a: { name?: string }) => a?.name ?? '').filter(Boolean)

function portOK(value: string) {
  const m = /^(?:(?:\d{1,3}(?:\.\d{1,3}){3}|\[[0-9a-fA-F:]+\]|localhost):)?(\d{1,5})$/.exec(value)
  return !!m && Number(m[1]) > 0 && Number(m[1]) <= 65535
}

export function setupError(doc: Document, f: AppSetup): string {
  if (!/^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(f.name.trim())) return '应用名只能用字母、数字、. _ -，字母或数字开头'
  if (appNames(doc).some((n) => n.toLowerCase() === f.name.trim().toLowerCase())) return `已经有叫 ${f.name.trim()} 的应用了`
  if (!f.path.trim()) return '请选择或填写产物路径'
  const targets = keysAt(doc, ['targets'])
  if (!f.targets.length || f.targets.some((t) => !targets.includes(t))) return '请选择目标服务器'
  if (hasAlias(doc, ['apps']) || targets.some((t) => hasAlias(doc, ['targets', t, 'apps']) || hasAlias(doc, ['targets', t, 'vars']))) return '应用或服务器使用了 YAML 别名，请在 YAML 页添加应用'
  if (!appNames(doc).length && f.targets.length !== targets.length) return '项目的第一个应用需要选择全部服务器；之后可分别设置应用范围'
  if (f.build === '@inline' && !f.buildCommand.trim()) return '请填写构建命令'
  if (f.build && f.build !== '@inline' && !keysAt(doc, ['builds']).includes(f.build)) return '选择的构建已不存在，请重新选择'
  if (f.type === 'static') {
    if (f.serve !== 'files' && !portOK(f.port.trim())) return '网站端口应为 1～65535，或 地址:端口'
    if (f.serve === 'installed' && f.targets.some((t) => !f.instances[t])) return '请为每台目标服务器选择已安装的 Nginx'
    if (f.serve !== 'files') {
      if (f.domains.split(/[\s,，]+/).filter(Boolean).some((d) => !/^[A-Za-z0-9_.*-]+$/.test(d))) return '请填写有效域名，多个域名用空格或逗号分开'
      const paths = new Set<string>()
      for (const p of f.proxies) {
        if (!p.path.trim() && !p.to.trim()) continue
        if (!/^\/[A-Za-z0-9._~/-]*$/.test(p.path.trim()) || !/^https?:\/\/[A-Za-z0-9.\-\[\]:]+(\/[A-Za-z0-9._~/-]*)?$/.test(p.to.trim())) return '接口转发需要 /api/ 这样的路径和 http(s):// 开头的后端地址'
        if (paths.has(p.path.trim())) return `接口转发路径 ${p.path.trim()} 重复`
        paths.add(p.path.trim())
      }
    }
  }
  if (f.type !== 'static' || f.serve === 'standalone') {
    if (f.user.trim() === 'root' || (f.user.trim() && !/^[a-z_][a-z0-9_-]*\$?$/.test(f.user.trim()))) return '运行用户须为非 root 的 Linux 用户'
    if (!f.user.trim() && f.targets.some((t) => !scalarAt(doc, ['targets', t, 'runAs']))) return '请填写运行用户；服务器上没有时会在部署时自动创建'
  }
  if (f.type === 'java' && f.javaSource === 'server' && f.targets.some((t) => !f.javaPaths[t])) return '请为每台服务器选择已安装的 JDK'
  if (['python', 'node', 'dotnet'].includes(f.type) && !lines(f.exec).length) return '请填写启动命令，每行一个参数'
  if (f.type !== 'static' && f.port.trim() && !portOK(f.port.trim())) return '健康检查端口应为 1～65535，或 地址:端口'
  for (const line of f.type === 'static' ? [] : lines(f.env)) {
    if (!/^[A-Za-z_][A-Za-z0-9_]*=/.test(line)) return '环境变量每行填写 KEY=值'
  }
  return ''
}

/** 写入应用和服务器范围；多台服务器的运行环境用独立变量绑定，避免改到已有应用。 */
export function applyAppSetup(doc: Document, f: AppSetup): number {
  const error = setupError(doc, f)
  if (error) throw new Error(error)
  const name = f.name.trim()
  const oldApps = appNames(doc)
  const app: Record<string, unknown> = { name, type: f.type, artifact: { path: f.path.trim() } }
  if (f.build === '@inline') app.build = { command: f.buildCommand.trim(), ...(f.buildWorkdir.trim() ? { workdir: f.buildWorkdir.trim() } : {}) }
  else if (f.build) app.build = f.build

  function perTarget(prefix: string, values: Record<string, string>): string {
    const unique = [...new Set(f.targets.map((t) => values[t]))]
    if (unique.length === 1) return unique[0]
    const base = `${prefix}_${name.replace(/[^A-Za-z0-9_]/g, '_')}`
    let key = base
    for (let i = 2; nodeAt(doc, ['vars', key]) !== undefined || keysAt(doc, ['targets']).some((t) => nodeAt(doc, ['targets', t, 'vars', key]) !== undefined); i++) key = `${base}_${i}`
    for (const t of f.targets) setScalar(doc, ['targets', t, 'vars', key], values[t])
    return '${' + key + '}'
  }

  if (f.type === 'static') {
    if (f.serve !== 'files') {
      app.static = {
        port: f.port.trim(), spa: f.spa,
        ...(f.serve === 'installed' ? { nginxInstance: perTarget('nginx', f.instances) } : {}),
        ...(f.domains.trim() ? { domains: f.domains.split(/[\s,，]+/).filter(Boolean) } : {}),
        ...(f.proxies.some((p) => p.path.trim()) ? { proxy: Object.fromEntries(f.proxies.filter((p) => p.path.trim()).map((p) => [p.path.trim(), p.to.trim()])) } : {}),
      }
      if (f.serve === 'standalone' && f.user.trim()) app.service = { user: f.user.trim() }
    }
  } else {
    const service: Record<string, unknown> = {}
    if (f.user.trim()) service.user = f.user.trim()
    if (f.type === 'java') {
      if (f.javaSource === 'ferry') service.runtime = { java: f.javaMajor }
      else service.java = perTarget('java', f.javaPaths)
      if (lines(f.jvmArgs).length) service.jvmArgs = lines(f.jvmArgs)
    }
    if (['java', 'binary'].includes(f.type) && lines(f.args).length) service.args = lines(f.args)
    if (['python', 'node', 'dotnet'].includes(f.type)) service.exec = lines(f.exec)
    if (lines(f.env).length) service.env = Object.fromEntries(lines(f.env).map((l) => { const i = l.indexOf('='); return [l.slice(0, i), l.slice(i + 1)] }))
    if (f.port.trim()) service.health = { tcp: f.port.trim() }
    if (Object.keys(service).length) app.service = service
  }
  for (const t of keysAt(doc, ['targets'])) {
    const allowed = listAt(doc, ['targets', t, 'apps'])
    if (f.targets.includes(t)) {
      if (allowed?.length) setList(doc, ['targets', t, 'apps'], [...allowed, name])
    } else if (!allowed?.length) setList(doc, ['targets', t, 'apps'], oldApps)
  }
  return append(doc, ['apps'], app)
}
