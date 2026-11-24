import { stringify } from 'yaml'
import type { ArchivePreview } from './ferry'

export type CustomReadyMode = 'tcp' | 'http' | 'process'
export type CustomServiceType = 'simple' | 'forking'

export interface CustomEnvRow {
  key: string
  value: string
}

export interface CustomInstallDraft {
  pkg: string
  title: string
  name: string
  inDir: string
  javaDir: string
  exec: string
  readyMode: CustomReadyMode
  port: number
  readyUrl: string
  readyTimeout: number
  user: string
  allowRoot: boolean
  serviceType: CustomServiceType
  workdir: string
  keepOnFail: boolean
  java: number
  format: string
  strip: number
  into: string
  envRows: CustomEnvRow[]
  configPath: string
  configContent: string
  logPath: string
  save: boolean
  id: string
}

export function createCustomDraft(name: string, inDir = '/opt/ferry-software', port = 8080): CustomInstallDraft {
  return {
    pkg: '',
    title: '',
    name,
    inDir: inDir || '/opt/ferry-software',
    javaDir: '',
    exec: '',
    readyMode: 'tcp',
    port,
    readyUrl: '',
    readyTimeout: 180,
    user: '',
    allowRoot: false,
    serviceType: 'simple',
    workdir: '',
    keepOnFail: false,
    java: 0,
    format: '',
    strip: 1,
    into: '',
    envRows: [],
    configPath: '',
    configContent: '',
    logPath: '',
    save: false,
    id: '',
  }
}

export function cloneCustomDraft(draft: CustomInstallDraft): CustomInstallDraft {
  return {
    ...draft,
    envRows: draft.envRows.map((row) => ({ ...row })),
  }
}

export function detectPackageFormat(file: string): string {
  const f = file.toLowerCase()
  if (f.endsWith('.tar.gz') || f.endsWith('.tgz')) return 'tar.gz'
  if (f.endsWith('.tar.xz') || f.endsWith('.txz')) return 'tar.xz'
  if (f.endsWith('.tar.bz2') || f.endsWith('.tbz2') || f.endsWith('.tbz')) return 'tar.bz2'
  if (f.endsWith('.tar.zst') || f.endsWith('.tzst')) return 'tar.zst'
  if (f.endsWith('.tar')) return 'tar'
  if (f.endsWith('.zip')) return 'zip'
  if (f.endsWith('.jar') || f.endsWith('.war')) return 'jar'
  if (f.endsWith('.rpm')) return 'rpm'
  if (f.endsWith('.deb')) return 'deb'
  return 'binary'
}

export function isArchiveFormat(format: string): boolean {
  return ['tar.gz', 'tar.xz', 'tar.bz2', 'tar.zst', 'tar', 'zip'].includes(format)
}

export function isSystemSingleFormat(format: string): boolean {
  return format === 'rpm' || format === 'deb'
}

export function nextFreeTcpPort(start: number, usedPorts: Set<number>): number {
  for (let p = Math.max(1, start); p <= Math.min(65535, start + 1000); p++) {
    if (!usedPorts.has(p)) return p
  }
  return start
}

export function deriveTitleFromFile(file: string): string {
  const base = file
    .replace(/(\.tar\.gz|\.tgz|\.tar\.xz|\.txz|\.tar\.bz2|\.tbz2|\.tbz|\.tar\.zst|\.tzst|\.tar|\.zip|\.jar|\.war|\.rpm|\.deb)$/i, '')
    .replace(/[-_.]?v?\d.*$/, '')
  return base || file
}

export interface CustomValidationContext {
  takenNames: Set<string>
  serverArch?: string
  archivePreview?: ArchivePreview
  inError?: string
  javaDirError?: string
  portTakenText?: (port: number) => string
}

export function validateCustomDraft(c: CustomInstallDraft, ctx: CustomValidationContext): Record<string, string> {
  const e: Record<string, string> = {}
  if (!c.pkg) {
    e.pkg = '选一个安装包（没有就先导入）'
  } else if (ctx.archivePreview?.bundle) {
    e.pkg = '系统软件包集合（ZIP），请在对应软件的系统包安装方式里选择离线包'
  } else if (isArchiveFormat(c.format) && ctx.archivePreview && ctx.archivePreview.entries === 0 && c.format !== 'tar.xz') {
    e.pkg = `当前剥离层数（${c.strip}）没有保留任何文件，建议改为 ${ctx.archivePreview.recommendedStrip}`
  } else if (
    isArchiveFormat(c.format) &&
    ctx.serverArch &&
    ctx.archivePreview?.arches?.length &&
    !ctx.archivePreview.arches.includes(ctx.serverArch)
  ) {
    e.pkg = `压缩包内程序架构为 ${ctx.archivePreview.arches.join('/')}，与目标服务器（${ctx.serverArch}）不匹配`
  }

  if (!/^[a-z][a-z0-9-]{0,30}$/.test(c.name)) {
    e.name = '小写字母、数字、横线，以字母开头'
  } else if (ctx.takenNames.has(c.name)) {
    e.name = '已经有同名的了'
  }

  if (!c.exec.trim()) {
    e.exec = '写启动命令'
  } else if (/^(?:"[^"]*\/"|'[^']*\/'|[^\s"'\\]+\/)(?:\s|$)/.test(c.exec.trim())) {
    e.exec = '启动命令要指向程序文件'
  } else if (c.exec.includes('${java}') && !c.java) {
    e.exec = '用了 ${java}：要选 JDK'
  }

  if (c.java && ctx.javaDirError) e.javaDir = ctx.javaDirError
  if (ctx.inError) e.in = ctx.inError

  if (c.readyMode !== 'process') {
    if (!c.port || c.port < 1 || c.port > 65535) {
      e.port = '端口要在 1～65535 之间'
    } else if (ctx.portTakenText) {
      const taken = ctx.portTakenText(c.port)
      if (taken) e.port = `${c.port} ${taken}`
    }
    if (c.readyMode === 'http') {
      const url = c.readyUrl.trim()
      if (!url) {
        e.readyUrl = '请填写本机 HTTP 检查地址，如 http://127.0.0.1:${port}/health'
      } else if (!/^https?:\/\/(127\.0\.0\.1|localhost)(:\d+|:\$\{port\})?(\/.*)?$/i.test(url)) {
        e.readyUrl = 'HTTP 检查地址只支持本机 127.0.0.1 或 localhost'
      }
    }
  }

  const user = c.user.trim()
  if (user && !/^[a-z_][a-z0-9_-]{0,31}$/.test(user)) {
    e.user = '运行用户只能用小写字母、数字、下划线和横线'
  } else if (user === 'root' && !c.allowRoot) {
    e.user = '运行用户为 root 时请勾选「允许以 root 运行」确认风险'
  }

  if (c.configPath.trim()) {
    const p = c.configPath.trim().replace(/^\/+/, '')
    if (p.includes('..') || !/^[A-Za-z0-9._/-]+$/.test(p)) {
      e.configPath = '配置文件路径请写安装目录下的相对路径，如 conf/config.yaml'
    }
  }

  for (const row of c.envRows) {
    const k = row.key.trim()
    if (!k && !row.value) continue
    if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(k)) {
      e.env = `环境变量名无效：${k || '（空）'}`
      break
    }
    if (/[\r\n]/.test(row.value)) {
      e.env = `环境变量 ${k} 的值不能包含换行符`
      break
    }
  }

  if (c.save && !/^[a-z][a-z0-9-]{0,30}$/.test(c.id)) {
    e.id = '模板 ID：小写字母、数字、横线，以字母开头'
  }
  return e
}

/** 根据自定义安装草稿生成 Ferry 软件模板 YAML */
export function buildCustomYAML(c: CustomInstallDraft): string {
  const id = c.save && c.id ? c.id : c.name
  const pkg: Record<string, unknown> = { format: c.format }
  if (isArchiveFormat(c.format)) {
    if (c.strip > 0) pkg.strip = c.strip
    // 压缩包若指定了子目录（或默认放到 app 子目录）可支持后续原地升级
    if (c.into.trim()) pkg.into = c.into.trim()
  } else if (c.into.trim()) {
    pkg.into = c.into.trim()
  }

  const effectiveUser = c.allowRoot ? 'root' : c.user.trim() || id.replace(/-/g, '_')
  const v: Record<string, unknown> = {
    label: isSystemSingleFormat(c.format) ? `导入的 ${c.format.toUpperCase()} 系统包` : '导入的安装包',
    user: effectiveUser,
    package: pkg,
  }
  if (isSystemSingleFormat(c.format)) {
    v.system = {}
  }
  if (c.java) v.java = c.java

  const workdir = c.workdir.trim()
    ? c.workdir.trim().startsWith('/')
      ? c.workdir.trim()
      : `\${dir}/${c.workdir.trim().replace(/^\/+/, '')}`
    : '${dir}'

  const ready: Record<string, unknown> = {}
  if (c.readyMode === 'process') {
    ready.mode = 'process'
    ready.stable = 5
  } else if (c.readyMode === 'http') {
    ready.mode = 'http'
    ready.port = '${port}'
    ready.url = c.readyUrl.trim() || 'http://127.0.0.1:${port}/'
  } else {
    ready.port = '${port}'
  }
  if (c.readyTimeout && c.readyTimeout !== 180) {
    ready.timeout = c.readyTimeout
  }

  const env: Record<string, string> = {}
  for (const row of c.envRows) {
    const k = row.key.trim()
    if (k) env[k] = row.value
  }

  const service: Record<string, unknown> = {
    exec: c.exec.trim().replace(/\s*\n\s*/g, ' '),
    workdir,
    ready,
  }
  if (Object.keys(env).length > 0) service.env = env
  if (c.serviceType === 'forking') service.type = 'forking'
  if (c.allowRoot) service.allowRoot = true
  if (c.keepOnFail) service.keepOnFail = true
  v.service = service

  const cfgPath = c.configPath.trim().replace(/^\/+/, '')
  if (cfgPath) {
    const fullCfg = `\${dir}/${cfgPath}`
    v.files = [{ path: fullCfg, content: c.configContent }]
    v.configs = [{ label: cfgPath.split('/').pop() || '配置文件', path: fullCfg, apply: 'restart' }]
  }

  const logPath = c.logPath.trim().replace(/^\/+/, '')
  if (logPath) {
    v.logs = [`\${dir}/${logPath}`]
  }

  const doc: Record<string, unknown> = {
    id,
    name: c.title.trim() || id,
    category: '自定义',
    desc: `自定义安装：${c.pkg}`,
    variants: [v],
  }
  if (c.readyMode !== 'process') {
    doc.params = [{ key: 'port', label: '端口', type: 'port', default: String(c.port) }]
  }
  return stringify(doc)
}
