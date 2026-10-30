// 调用内置的 ferry 命令行（Tauri sidecar）。--json 模式下每行一个 JSON，最后一行 kind=result。
import { spawnFerry } from './platform'

export interface FerryLine {
  kind: 'step' | 'info' | 'warn' | 'error' | 'event' | 'progress' | 'confirm-required' | 'result' | string
  time?: string
  msg?: string
  level?: string
  code?: string
  app?: string
  target?: string
  label?: string
  done?: number
  total?: number
  ok?: boolean
  data?: unknown
  error?: string
  errorDetail?: ErrorDetail
}

/** 结构化的错误信息：需要密码、密码被拒绝、未知主机…… */
export interface ErrorDetail {
  kind: 'password-required' | 'password-rejected' | 'passphrase-required' | 'passphrase-rejected' | 'unknown-host' | 'key-missing' | string
  target?: string
  key?: string
  env?: string
  account?: string
  prompt?: string
  addr?: string
  fingerprint?: string
  /** java-mismatch：哪个应用、需要的 / 现在的 Java 大版本 */
  app?: string
  need?: number
  have?: number
  /** busy-artifacts：本机正被程序占着的产物 */
  items?: BusyItem[]
  /** key-missing：私钥来自本机设置（不是 ferry.yaml） */
  local?: boolean
  /** files-exist：要传的文件在那边已经有了 */
  names?: string[]
  /** blocked-dirs：运行用户 user 进不了的上级目录 */
  user?: string
  dirs?: string[]
}

/** 被本机程序占着的产物（Windows 锁住正在运行的 jar） */
export interface BusyItem {
  app: string
  path: string
  holders: { pid: number; name: string; cmd?: string }[]
}

/** 本机 JDK 库里的一个 JDK */
export interface JdkMeta {
  id: string
  java: number
  version: string
  arch: 'x64' | 'aarch64'
  vendor?: string
  sha256: string
  size: number
  source?: string
  added: string
  path?: string
}

export interface JdkList {
  dir: string
  source: string
  jdks: JdkMeta[]
  nginxDir: string
  nginxSource: string
  nginx: NginxMeta[]
}

/** 本机库里的一个 nginx（静态链接的单个程序） */
export interface NginxMeta {
  id: string
  version: string
  series: string
  arch: 'x86_64' | 'aarch64' | 'ppc64le'
  sha256: string
  size: number
  source?: string
}

export interface FerryResult<T> {
  ok: boolean
  data?: T
  error?: string
  detail?: ErrorDetail
}

export interface Running<T> {
  done: Promise<FerryResult<T>>
  kill: () => Promise<void>
}

/** 本次运行期间用户输入、但没有要求记住的密码（环境变量名 → 值），每次调用 ferry 都带上。 */
export const sessionEnv: Record<string, string> = {}

/** 运行 ferry <args> --json，逐行回调，返回最终结果。 */
export function ferry<T = unknown>(args: string[], onLine?: (l: FerryLine) => void, env: Record<string, string> = {}): Running<T> {
  let result: FerryResult<T> | undefined
  let killed = false
  const stderr: string[] = []
  const proc = spawnFerry(
    [...args, '--json'],
    (raw) => {
      const line = raw.trim()
      if (!line) return
      let obj: FerryLine
      try {
        obj = JSON.parse(line)
      } catch {
        onLine?.({ kind: 'info', msg: line })
        return
      }
      if (obj.kind === 'result') result = { ok: !!obj.ok, data: obj.data as T, error: obj.error, detail: obj.errorDetail }
      onLine?.(obj)
    },
    (l) => l.trim() && stderr.push(l.trim()),
    { ...sessionEnv, ...env },
  )
  const done = proc.done.then((code): FerryResult<T> => {
    if (result) return result
    return { ok: false, error: killed ? '已取消' : stderr.join('\n') || `ferry 异常退出（退出码 ${code}）` }
  })
  return {
    done,
    kill: async () => {
      killed = true
      await proc.kill()
    },
  }
}

/** 一次性调用，直接拿结果；失败抛出 FerryError。 */
export async function call<T = unknown>(args: string[], env: Record<string, string> = {}): Promise<T> {
  const r = await ferry<T>(args, undefined, env).done
  if (!r.ok) throw new FerryError(r.error ?? '未知错误', r.detail)
  return r.data as T
}

/** 非 JSON 模式（日志）：原样逐行输出。 */
export function ferryRaw(args: string[], onLine: (line: string) => void): Running<void> {
  let killed = false
  const stderr: string[] = []
  const proc = spawnFerry(args, onLine, (l) => l.trim() && stderr.push(l.trim()), { ...sessionEnv })
  const done = proc.done.then((code): FerryResult<void> =>
    code === 0 || killed ? { ok: true } : { ok: false, error: stderr.join('\n') || `退出码 ${code}` },
  )
  return {
    done,
    kill: async () => {
      killed = true
      await proc.kill()
    },
  }
}

/** 一个打开着的服务器终端（ferry terminal）。 */
export interface TerminalProc {
  /** 结束（shell 退出、连接断开、关闭）；失败时带原因（需要密码、未知主机……） */
  done: Promise<FerryResult<{ exit: number }>>
  input: (data: string) => void
  /** xterm 的 onBinary：每个字符是一个字节 */
  inputBinary: (data: string) => void
  resize: (cols: number, rows: number) => void
  /** 显示不过来时暂停输出（服务器上的程序也跟着停下），显示完再继续 */
  flow: (pause: boolean) => void
  close: () => Promise<void>
}

/** 打开目标的交互式终端：连接过程（kind=step / info）与 terminal-open 由 onLine 告知，终端输出交给 onData。 */
export function openTerminal(
  config: string,
  target: string,
  size: { cols: number; rows: number },
  onLine: (l: FerryLine) => void,
  onData: (data: string) => void,
): TerminalProc {
  let result: FerryResult<{ exit: number }> | undefined
  let closed = false
  const stderr: string[] = []
  const proc = spawnFerry(
    ['terminal', target, '-c', config, '--cols', String(size.cols), '--rows', String(size.rows), '--json'],
    (raw) => {
      let obj: FerryLine
      try {
        obj = JSON.parse(raw)
      } catch {
        return
      }
      if (obj.kind === 'result') result = { ok: !!obj.ok, data: obj.data as { exit: number }, error: obj.error, detail: obj.errorDetail }
      else onLine(obj)
    },
    (l) => l.trim() && stderr.push(l.trim()),
    { ...sessionEnv },
    onData,
  )
  return {
    done: proc.done.then((code) => result ?? { ok: closed, error: stderr.join('\n') || `ferry 异常退出（退出码 ${code}）` }),
    input: (data) => proc.post('input', { data }),
    inputBinary: (data) => proc.post('input', { b64: btoa(data) }),
    resize: (cols, rows) => proc.post('resize', { cols, rows }),
    flow: (pause) => proc.post('flow', { pause }),
    close: async () => {
      closed = true
      await proc.kill()
    },
  }
}

/** 「新建连接」保存的服务器（ferry connection list） */
export interface Connection {
  /** 在 connections.yaml 里的名字（命令、凭据、主机公钥核对用） */
  name: string
  /** 显示名 */
  title: string
  host: string
  port: number
  user: string
  auth: 'key' | 'password'
  identityFile: string
  hostKey: string
}

/** 服务器上目录里的一项（ferry files ls） */
export interface FileEntry {
  name: string
  size: number
  mode: string
  dir: boolean
  link?: boolean
  mtime: string
}

export interface DirListing {
  path: string
  home: string
  entries: FileEntry[]
}

/** 只读文本预览（ferry files preview）；content 最多读取服务器文件的前 limit 字节。 */
export interface FilePreview {
  path: string
  size: number
  content: string
  encoding: string
  truncated: boolean
  limit: number
  unsupported?: string
  revision?: string
  bom: boolean
  lineEnding: 'LF' | 'CRLF' | 'CR'
  readOnly?: string
  backup?: boolean
}

export class FerryError extends Error {
  constructor(
    message: string,
    public detail?: ErrorDetail,
  ) {
    super(message)
  }

  /** 未核对过主机公钥（需要先 trust） */
  get unknownHost(): boolean {
    return this.detail?.kind === 'unknown-host' || this.message.includes('未知主机')
  }

  /** 指定的私钥不在这台电脑上，默认私钥也登录不了 */
  get keyMissing(): boolean {
    return this.detail?.kind === 'key-missing'
  }

  /** 需要（或重新输入）密码 / 私钥密码 */
  get needSecret(): boolean {
    return isSecretKind(this.detail?.kind)
  }
}

export function isSecretKind(kind?: string): boolean {
  return !!kind && /^(password|passphrase)-(required|rejected)$/.test(kind)
}

/** 与 ferry 的 secret.PasswordEnv 一致：FERRY_PASSWORD_<目标名大写，非字母数字换成 _> */
export function passwordEnv(target: string): string {
  return 'FERRY_PASSWORD_' + target.toUpperCase().replace(/[^A-Z0-9]/g, '_')
}

// ---------------------------------------------------------------- 数据类型（与 Go 端 JSON 一致）

export interface InspectTarget {
  name: string
  host: string
  port: number
  user: string
  sudo: boolean
  root: string
  runAs: string
  arch: string
  keepReleases: number
  confirm: boolean
  hostKey: string
  apps: string[]
  auth: 'key' | 'password'
  identityFile: string
  /** 单独设置了部署目录的应用 → 在这个目标上的实际目录 */
  appRoots?: Record<string, string>
  /** 生产服务器：AI 助手每个改动都要确认 */
  production?: boolean
  /** 允许 AI 助手改动（false = 只能查看） */
  aiChanges?: boolean
  /** 这台电脑的本机设置（登录方式、私钥）；没有 = 按 ferry.yaml */
  local?: LocalTarget | null
  /** 本机设置里只对这台服务器改了值的变量 */
  localVars?: string[]
  /** 指定的私钥在这台电脑上不存在 */
  keyMissing?: boolean
}

/** 一个目标的本机设置（只对这台电脑，不写进 ferry.yaml） */
export interface LocalTarget {
  auth?: 'key' | 'password'
  /** 私钥；auth 为 key 且没有私钥 = 用默认私钥、ssh-agent */
  identityFile?: string
  /** 这台服务器上变量的值（只对这台） */
  vars?: Record<string, string>
}

/** 本机设置里的一个变量：shared 是不改时的值，local 是本机设置的值 */
export interface LocalVar {
  name: string
  shared: string
  local: string | null
  /** 用在这台电脑上（源码目录等），所有服务器共用 */
  localUse?: boolean
}

/** 这台电脑上缺的东西：私钥、构建目录 */
export interface MachineProblem {
  kind: 'key-missing' | 'workdir-missing' | string
  target?: string
  build?: string
  path: string
  /** 构建目录里用到的项目变量（可以在本机设置里改） */
  vars?: string[]
  msg: string
}

/** ferry local --json */
export interface LocalReport {
  file: string
  targets: {
    name: string
    auth: 'key' | 'password'
    identityFile: string
    keyPath: string
    keyMissing: boolean
    sharedAuth: 'key' | 'password'
    sharedIdentityFile: string
    local: LocalTarget | null
    /** 这台服务器的变量（本机设置里只对它的值） */
    vars: LocalVar[]
  }[]
  /** 所有服务器共用的变量 */
  vars: LocalVar[]
  problems: MachineProblem[]
}

export interface InspectApp {
  name: string
  type: 'java' | 'binary' | 'static' | 'python' | 'node' | 'dotnet'
  order: number
  artifact: string
  /** 应用自己的部署根目录（原样，可能含变量） */
  root?: string
  build?: string
  unit?: string
}

export interface Inspect {
  project: string
  path: string
  dir: string
  targets: InspectTarget[]
  apps: InspectApp[]
  /** 值来自本机设置的变量 */
  localVars?: string[]
  /** 这台电脑上缺的东西 */
  problems?: MachineProblem[]
}

export interface ReleaseMeta {
  id: string
  app: string
  sha256: string
  size: number
  commit?: string
  branch?: string
  dirty?: boolean
  builtAt?: string
  deployedAt?: string
  deployedBy?: string
  source?: string
  /** 附带文件放到版本目录里的位置 */
  files?: string[]
}

/** 正拿着部署锁的操作：谁、在做什么 */
export interface LockHolder {
  actor: string
  action: string
  apps?: string[]
  since: string
}

export interface AppStatus {
  name: string
  /** 现在有人在这台服务器上部署、回滚、启停 */
  busy?: LockHolder
  type: string
  unit?: string
  active?: string
  sub?: string
  pid?: number
  since?: string
  healthy?: boolean
  current?: ReleaseMeta
  releases: number
  problem?: string
  /** 正在监听的 TCP 地址，如 127.0.0.1:9201、*:8080 */
  listen?: string[]
  /** 配置里健康检查用的端口（没运行时显示） */
  port?: string
  /** 附加端口：服务还在监听的其他地址及其用途 */
  extra?: ExtraPort[]
  /** 静态站点：nginx 里 root / alias 指向它的 server */
  sites?: Site[]
  /** 静态站点没找到 nginx 站点时的原因 */
  serving?: string
}

/** ferry software：服务器上已有的软件（只读查看） */
export interface SoftwareReport {
  items: Software[]
  /** 空 = 没装 Docker；ok = 已读取容器；其他为读取不了的原因 */
  docker?: string
  notes?: string[]
  /** 安装软件时用：服务器架构、Ferry 提供的 JDK、各目录剩余空间、已占用的端口 */
  arch?: string
  os?: { id: string; like?: string[]; version?: string; pretty?: string }
  runtimes?: Record<string, { id: string; java: number }>
  /** 服务器上 ferry 组件的目录：公共的 JDK 在它下面的 runtimes */
  ferryDir?: string
  disks?: { path: string; freeMB: number }[]
  ports?: number[]
}

/** ferry who：服务器上一个已登录的 SSH 连接 */
export interface SSHSession {
  /** 连接最上面的 sshd 进程（断开时用它） */
  pid: number
  user: string
  /** 对方 IP；看不到时没有 */
  from?: string
  fromPort?: number
  /** 这个连接开的终端（pts/0……） */
  ttys?: string[]
  /** 连上的时间（Unix 秒） */
  since: number
  /** 终端多久没有输入（秒）；没有终端时 -1 */
  idle: number
  /** 正在运行的程序 */
  cmds?: string[]
  kind: 'terminal' | 'sftp' | 'ferry' | 'exec' | 'idle'
  /** 本机 Ferry 正在用的连接：不能断开 */
  self?: boolean
  /** 登录用户能不能断开它 */
  kick: boolean
}

/** ferry server-info：Linux 服务器的一次只读采样；容量单位为字节。 */
export interface ServerInfo {
  hostname: string
  os: string
  kernel: string
  arch: string
  virtualization: string
  timezone: string
  now: number
  uptime: number
  cpu: { model: string; cores: number; usage: number | null; load: number[] | null }
  memory: { total: number; used: number; available: number; free: number; cached: number }
  swap: { total: number; used: number; free: number }
  mounts: ServerMount[]
  addresses: { interface: string; family: string; address: string }[]
  notes?: string[]
}

export interface ServerMount {
  device: string
  path: string
  fsType: string
  total: number
  used: number
  available: number
  usage: number | null
  inodes: number
  inodesUsed: number
  inodesUsage: number | null
  temporary: boolean
}

/** ferry who：服务器上连着的 SSH */
export interface SSHReport {
  priv: 'root' | 'sudo' | 'none'
  me: string
  sessions: SSHSession[]
  /** 正在登录、还没通过验证的连接数 */
  pending?: number
  notes?: string[]
  /** 服务器上的当前时间（Unix 秒） */
  now: number
}

/** ferry port：服务器上的端口与防火墙 */
export interface PortReport {
  firewall: Firewall
  ports: PortInfo[]
  /** 有 Docker 映射的端口时：能不能在 Docker 的 DOCKER-USER 规则链里拦外部访问 */
  dockerGuard?: { ok: boolean; iface?: string; why?: string }
  notes?: string[]
}

export interface Firewall {
  /** firewalld | ufw | iptables | nftables；空 = 没找到防火墙 */
  kind?: string
  active: boolean
  /** Ferry 能不能放行 / 关闭端口 */
  manage: boolean
  zone?: string
  /** 不能改的原因 */
  why?: string
  /** 放行的规则（给人看） */
  rules?: string[]
  /** 装了没开、Ferry 能打开 */
  enable?: boolean
  /** 在运行、Ferry 能关掉 */
  disable?: boolean
  /** Docker 正在运行（打开、关闭 firewalld 后要重启它） */
  docker?: boolean
}

export interface PortInfo {
  port: number
  proto: 'tcp' | 'udp'
  /** 监听的地址（*、127.0.0.1……）；没有程序在监听时为空 */
  listen?: string[]
  pid?: number
  process?: string
  unit?: string
  /** unit 是 Ferry 生成的（部署的应用、装的软件） */
  ferry?: boolean
  /** Docker 映射到主机的端口（防火墙规则管不住） */
  docker?: boolean
  container?: string
  /** SSH 端口：不能关闭 */
  ssh?: boolean
  /** 监听的是具体地址时，地址所在的网卡 */
  iface?: string
  /** 只在内部的虚拟网卡（libvirt、Docker 网桥……）上监听：外部连不到 */
  internal?: boolean
  /** open 放行 | blocked 拦截 | partial 只放行部分来源；空 = 没有防火墙或看不出来 */
  firewall?: '' | 'open' | 'blocked' | 'partial'
  rule?: string
  /** 放行只在这次开机有效 */
  temp?: boolean
  /** Docker 映射：容器里的端口 */
  ctrPort?: number
  /** Docker 映射：Ferry 在 DOCKER-USER 里拦着外部访问 */
  guard?: boolean
  /** 容器是谁建的：1Panel | compose；空 = docker run 或认不出 */
  manager?: '1Panel' | 'compose' | ''
  composeFile?: string
  composeDir?: string
  composeService?: string
}

/** ferry port check：从本机连服务器端口的结果 */
export interface PortProbe {
  port: number
  ok: boolean
  ms: number
  why?: 'refused' | 'timeout' | 'error'
  msg?: string
}

/** 软件模板（ferry software templates） */
export interface SwParam {
  key: string
  label: string
  type: 'text' | 'port' | 'password' | 'bool' | 'memory' | 'select' | 'number'
  default?: string
  options?: string[]
  help?: string
  /** port：还要占用的端口（主端口加上这些数） */
  offsets?: number[]
}

export interface SwVariant {
  label: string
  os?: string[]
  arch?: string[]
  note?: string
  java?: number
  user: string
  package?: { url?: string; file?: string; github?: string; fallback?: string; size?: string; format?: string; strip?: number; into?: string }
  system?: { dnf?: string[]; apt?: string[]; disable?: string[]; require?: string }
  files?: { path: string }[] | null
  steps?: { run?: string; http?: { post: string } }[] | null
  service?: { exec: string }
}

export interface SwTemplate {
  id: string
  name: string
  category: string
  desc: string
  homepage?: string
  params: SwParam[] | null
  variants: SwVariant[]
  builtin: boolean
  file?: string
  overrides?: boolean
  /** ai = AI 助手保存的 */
  origin?: string
}

/** 本机库里的安装包 */
export interface SwPackage {
  id: string
  template?: string
  version?: string
  file: string
  size: number
  sha256: string
  /** 「导入：本机路径」或下载地址 */
  source?: string
  added?: string
  /** tar.gz / tar.xz / zip / jar / binary */
  format?: string
  /** binary 是 Linux 程序时：amd64 / arm64 */
  arch?: string
  /** 给人看的类型：Linux 程序（arm64）、tar.gz 压缩包…… */
  desc?: string
}

/** 安装包什么时候、怎么加进来的；path = 导入时的本机路径 */
export function pkgOrigin(m: SwPackage): { text: string; path: string } {
  const s = m.source ?? ''
  const join = (...a: string[]) => a.filter(Boolean).join(' ')
  if (s.startsWith('导入：')) return { text: join(time(m.added), '导入'), path: s.slice(3) }
  if (/^https?:\/\//.test(s)) return { text: join(time(m.added), `从 ${new URL(s).host} 下载`), path: '' }
  return { text: join(time(m.added), s), path: '' }
}

export interface SwTemplates {
  templates: SwTemplate[]
  errors: string[] | null
  dir: string
  library: SwPackage[] | null
}

/** 变体能不能用在这台服务器上（与 Go 的 Variant.Fits 一致）；能用返回空 */
export function variantMisfit(v: SwVariant, os: SoftwareReport['os'], arch?: string): string {
  if (arch && v.arch?.length && !v.arch.includes(arch)) return `不支持 ${arch} 架构`
  if (os && v.os?.length) {
    const ids = [os.id, ...(os.like ?? [])].map((x) => x.toLowerCase())
    if (!v.os.some((x) => ids.includes(x.toLowerCase()))) return `只适用于 ${v.os.join('、')} 系列，这台服务器是 ${os.pretty || os.id}`
  }
  return ''
}

/** ferry software install 的结果 */
export interface SoftwareResult {
  name: string
  kind: string
  title?: string
  version: string
  dir: string
  unit: string
  user: string
  ports: Record<string, number>
  public?: boolean
  notes?: string[]
  target: string
  password?: string
  saved: boolean
}

export interface Software {
  /** mysql、redis、nacos……；认不出的容器为空 */
  kind?: string
  name: string
  version?: string
  state: 'running' | 'stopped'
  /** systemd 系统服务 | docker 容器 | process 直接启动的进程 */
  via: 'systemd' | 'docker' | 'process'
  unit?: string
  container?: string
  image?: string
  /** 容器状态原文，如 Exited (0) 3 days ago */
  status?: string
  /** 1Panel、宝塔面板、Ferry、Docker Compose（项目）…… */
  manager?: string
  pid?: number
  since?: string
  /** 主机上能访问的监听地址 */
  listen?: string[]
  /** 容器里监听、没映射到主机的端口 */
  inner?: string[]
  path?: string
  /** Ferry 安装的：实例名（可以启停、看日志、卸载）与安装目录 */
  instance?: string
  dir?: string
  hosting?: {binary: string; config: string; prefix: string; sites: string}
}

export interface SoftwareConfigFile { id: string; label: string; path: string; source?: string; format?: string; readOnly?: string; apply?: 'reload' | 'restart'; backup: boolean; inPlace?: boolean }
export interface SoftwareConfigs { files: SoftwareConfigFile[]; notes?: string[]; software: Software }

export interface ExtraPort {
  addr: string
  /** 用途；识别不出时为空 */
  use?: string
  /** 不宜对外开放（没有认证，或能控制进程） */
  risky?: boolean
}

export interface Site {
  listen: string[]
  ssl?: boolean
  names?: string[]
  location?: string
  file?: string
}

export interface ReleaseInfo extends ReleaseMeta {
  current: boolean
  failed: boolean
  /** 归档了几次运行的日志 */
  logRuns?: number
}

export interface AppReleases {
  app: string
  releases: ReleaseInfo[] | null
  legacy: boolean
}

export interface HistoryEntry {
  time: string
  action: string
  app: string
  release?: string
  result: string
  commit?: string
  sha256?: string
  actor?: string
  msg?: string
}

export interface AppOutcome {
  app: string
  release?: string
  result: string
  msg?: string
  /** 从启动到就绪用了几秒；prevReadySec 上一次成功部署时用了几秒 */
  readySec?: number
  prevReadySec?: number
  /** 启动期间服务输出里的错误行数；prevErrorLines 上一次的（-1 = 没有记录） */
  errorLines?: number
  prevErrorLines?: number
}

/** 部署会带上线的改动（ferry changes） */
export interface AppChange {
  app: string
  online?: { id: string; commit?: string; branch?: string; deployedAt?: string; deployedBy?: string }
  local: { commit: string; branch: string; dirty: boolean }
  known: boolean
  ahead: number
  behind: number
  commits: { hash: string; author: string; date: string; subject: string }[] | null
  note?: string
  warn?: boolean
  /** 生成的 systemd 单元和线上比变了的行（「+ 」新增、「- 」去掉） */
  unitDiff?: string[] | null
  filesAdded?: string[] | null
  filesChanged?: string[] | null
  filesRemoved?: string[] | null
}

export interface DeployReport {
  target: string
  release: string
  git: { commit: string; branch: string; dirty: boolean }
  apps: AppOutcome[] | null
  upload: number
  total: number
  elapsed: number
}

export interface KeyGenResult {
  target: string
  keyPath: string
  generated: boolean
  installed: boolean
  fingerprint: string
  configUpdated: boolean
  localUpdated: boolean
  configValue: string
}

export interface HostKey {
  target: string
  host: string
  port: number
  type: string
  fingerprint: string
  state: 'known' | 'unknown' | 'mismatch'
  pinned: string
}

// ---------------------------------------------------------------- 格式化

export function bytes(n: number): string {
  if (n >= 1 << 30) return (n / (1 << 30)).toFixed(2) + ' GB'
  if (n >= 1 << 20) return (n / (1 << 20)).toFixed(1) + ' MB'
  if (n >= 1 << 10) return (n / (1 << 10)).toFixed(0) + ' KB'
  return n + ' B'
}

export function shortCommit(c?: string, dirty?: boolean): string {
  if (!c) return ''
  return c.slice(0, 8) + (dirty ? '*' : '')
}

export function time(t?: string): string {
  if (!t || t.startsWith('0001')) return ''
  const d = new Date(t)
  if (isNaN(d.getTime())) return t
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
}

/** 新建项目：识别出的一个应用（路径相对项目目录） */
export interface Suggestion {
  name: string
  kind: string
  type: 'java' | 'binary' | 'static' | 'python' | 'node' | 'dotnet'
  workdir: string
  command: string
  env?: Record<string, string>
  artifact: string
  port?: number
  /** 需要的 Java 版本（从 pom 推断） */
  java?: number
  /** 前端的接口转发（从开发代理推断）：路径前缀 → 后端地址 */
  proxy?: Record<string, string>
  module?: string
  buildGroup?: string
  groupCommand?: string
  notes?: string[]
  /** 工程目录里看着像配置的文件、目录（相对项目目录）：附带文件的候选 */
  configFiles?: string[]
  /** python、node、dotnet：启动命令、服务的环境变量、不上传的 */
  exec?: string[]
  serviceEnv?: Record<string, string>
  exclude?: string[]
  /** AI 识别新加或改过的（要用户核对）和它的判断依据 */
  ai?: boolean
  reason?: string
}

/** 新建项目：目录的识别结果 */
export interface Detected {
  dir: string
  project: string
  git: boolean
  /** 目录里已有的 ferry.yaml */
  config: string
  apps: Suggestion[]
}

/** AI 识别的结果（ferry detect --ai）：应用列表、模型的总结、用量 */
export interface AiDetected extends Detected {
  summary: string
  usage: { input: number; cacheRead?: number; cacheWrite?: number; output: number }
  model?: string
}

/** 某个版本一次运行的日志（当前运行，或切换版本时归档的） */
export interface LogRun {
  release: string
  /** 归档名；当前运行没有 */
  id?: string
  start?: string
  end?: string
  current?: boolean
  files: { path: string; size: number; mtime?: string }[]
  /** 这次运行的日志在服务器上的目录（文件路径相对它，如 logs/app.log）；以前压缩的归档没有 */
  dir?: string
  size: number
}

/** 目标的常驻连接状态（ferry serve 的 pool 命令） */
export interface ConnStatus {
  target: string
  connected: boolean
  label?: string
  since?: string
  lastUsed?: string
  inUse: number
  /** 到服务器的往返时间（毫秒）；slow：量的时候超过 3 秒还没回来 */
  rtt?: number
  slow?: boolean
}

/** 到服务器的往返时间怎么显示：文字和快慢（100 毫秒以内顺畅，300 毫秒以内能感觉到，再慢就明显卡了） */
export function latencyInfo(ms?: number, slow?: boolean): { text: string; level: 'good' | 'fair' | 'poor' } | undefined {
  if (slow) return { text: '超过 3 秒', level: 'poor' }
  if (ms === undefined || ms <= 0) return undefined
  const text = ms < 10 ? `${ms.toFixed(1)} ms` : `${Math.round(ms)} ms`
  return { text, level: ms < 100 ? 'good' : ms < 300 ? 'fair' : 'poor' }
}

/** 往返时间的说明（鼠标悬停时看） */
export function latencyTip(level?: 'good' | 'fair' | 'poor'): string {
  const base = '到服务器的网络往返时间：在终端里打一个字，要等这么久才显示出来；每 5～10 秒量一次。'
  if (level === 'good') return base + '现在很顺畅。'
  if (level === 'fair') return base + '打字能感觉到一点延迟。'
  if (level === 'poor') return base + '网络较慢，打字会明显卡顿（和 Ferry 无关，换别的 SSH 工具也一样）。'
  return base
}

// ---------------------------------------------------------------- 设置

/** 设置 → AI 里的一个模型（API Key 在系统凭据库，按 id 区分） */
export interface AiModel {
  id: string
  name: string
  provider: 'anthropic' | 'openai'
  baseUrl: string
  model: string
  /** 默认思考强度：'' = 不设置 | off | on | low | medium | high | xhigh | max */
  effort: string
  /** OpenAI 兼容接口怎么传思考设置：'' = 自动识别 | effort | deepseek | qwen | none */
  thinking: string
  /** 能不能看图片：'' = 自动识别 | on | off */
  images?: string
  /** 单价（每百万 tokens，0 = 没填）：输入、命中缓存的输入、输出；currency 为 ¥ 或 $ */
  priceIn: number
  priceCache: number
  priceOut: number
  currency: string
}

export interface AppSettings {
  // statusMinutes：自动读取应用状态的间隔（分钟），-1 = 不自动读取
  connect: { timeoutSec: number; acceptNewHostKey: boolean; keyDir: string; uploadStreams: number; idleMinutes: number; statusMinutes: number }
  build: { env: Record<string, string> | null; path: string[] | null; verbose: boolean }
  logs: { buildLogDays: number }
  deploy: { skipBuild: boolean; full: boolean; keepPerms: boolean }
  runtime: { source: string; cacheDir: string; nginxSource: string; githubMirror: string; mysqlSource: string }
  ui: { theme: 'light' | 'dark' | 'system'; taskKeep: number; taskKeepDays: number; taskMaxMB: number; confirmClose: boolean; notify: boolean; glass: boolean; glassTransparency: number }
  ai: { models: AiModel[]; default: string; keepDays: number; maxMB: number }
  /** targets：只允许这些目标（「项目/目标」）；空 = 全部非生产目标 */
  mcp: { allowChanges: boolean; targets: string[] | null; logKeepDays: number; logMaxMB: number }
  notify: { channels: NotifyChannel[] }
  desktop: { projects: { path: string; name: string }[] | null; current: string }
  team: { name: string }
}

/** 部署通知的一种方式（设置 → 通知）。地址、加签密钥存在系统凭据库 */
export interface NotifyChannel {
  id: string
  name: string
  kind: 'wecom' | 'dingtalk' | 'feishu' | 'webhook'
  /** success、failure；空 = 都发 */
  on: string[] | null
  /** 只发这些项目（或「项目/目标」）的；空 = 全部 */
  scope: string[] | null
  productionOnly: boolean
  off: boolean
}

export const NOTIFY_KINDS: Record<NotifyChannel['kind'], string> = { wecom: '企业微信', dingtalk: '钉钉', feishu: '飞书', webhook: '自定义接口' }

export interface DataDirInfo {
  dir: string
  mode: 'env' | 'custom' | 'install' | 'user'
  defaultDir: string
  installDir?: string
  /** 没有用安装目录的原因 */
  note?: string
}

export interface SettingsResponse {
  settings: AppSettings
  dataDir: DataDirInfo
  file: string
  keyDir: string
  legacyDir: string
}

export interface SecretEntry {
  account: string
  kind: 'password' | 'passphrase' | string
  label: string
  savedAt: string
}

/** 事件代码的中文名称（代码本身留在协议里，程序按代码判断） */
const EV_LABELS: Record<string, string> = {
  INFO: '信息',
  WARN: '注意',
  LOCK: '加锁',
  CACHE: '缓存',
  ASSEMBLE: '组装',
  VERIFY: '校验',
  RELEASE: '新版本',
  STOP: '停止',
  SWITCH: '切换',
  UNIT: '单元文件',
  START: '启动',
  WAIT: '等待就绪',
  READY: '已就绪',
  CRASH: '崩溃',
  FAIL: '失败',
  DIAG: '诊断',
  ROLLBACK: '回滚',
  ABORT: '中止',
  PRUNE: '清理',
  RELOAD: '重新加载',
  HOOK: '部署命令',
  CERT: '证书',
  LOGS: '日志',
  RUNTIME: '运行环境',
  SOFTWARE: '软件',
  FIREWALL: '防火墙',
}
export const evLabel = (code?: string) => (code ? EV_LABELS[code] ?? code : '')

/** 按显示宽度补空格（中文占两格），用于等宽字体里对齐 */
export function padWidth(s: string, n: number): string {
  let w = 0
  for (const ch of s) w += /[\u1100-\u115f\u2e80-\ua4cf\uac00-\ud7a3\uf900-\ufaff\ufe30-\ufe4f\uff00-\uff60\uffe0-\uffe6]/.test(ch) ? 2 : 1
  return w < n ? s + ' '.repeat(n - w) : s
}

/** ferry logs --grep 的结果 */
export interface LogSearch {
  matches: { file: string; line: number; text: string; before?: string[]; after?: string[] }[]
  files: number
  truncated: boolean
}
