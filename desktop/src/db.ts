// 「数据库」：调用 ferry db …（internal/dbx）。连接、库、表、查询结果的类型和调用都在这里。
import { reactive, ref, watch, type Ref } from 'vue'
import { call, ferry, FerryError, isSecretKind, sessionEnv, type ErrorDetail, type Software } from './ferry'
import { secretRequest, trustRequest } from './state'
import { dbEndpoint, type Endpoint } from './softwareWeb'

export type DbKind = 'mysql' | 'postgres' | 'redis'

export interface DbVia {
  config: string
  target: string
  title?: string
}

export interface DbConn {
  id: string
  name: string
  kind: DbKind
  host: string
  port: number
  user: string
  database?: string
  via?: DbVia | null
  container?: string
  software?: string
  readOnly?: boolean
  /** 允许 AI 查询（编程软件经 MCP、Ferry 的 AI 助手）：只读 */
  ai?: boolean
  /** 也允许 AI 修改（插入、修改、删除数据，建表改表）：每次都要用户看过 SQL 同意 */
  aiWrite?: boolean
  /** AI 用这个账号连（建议是只读账号）；空 = 和上面的账号一样 */
  aiUser?: string
  /** 加密：空 = 自动 | off | require（不验证证书）| verify（验证证书） */
  tls?: '' | 'off' | 'require' | 'verify'
  savePassword?: boolean
  color?: string
  created?: string
}

export interface DbDatabase { name: string; system?: boolean; /** Redis：键的个数 */ keys?: number }
export interface DbTable {
  schema?: string
  name: string
  type: 'table' | 'view'
  rows?: number
  size?: number
  comment?: string
  engine?: string
  // 下面这些只有「对象」列表（tables 带 detail）才有
  autoInc?: number
  dataLen?: number
  indexLen?: number
  created?: string
  updated?: string
  collation?: string
  owner?: string
}
/** 函数或存储过程 */
export interface DbRoutine {
  schema?: string
  name: string
  type: 'function' | 'procedure'
  /** 参数：MySQL「IN a INT, OUT b VARCHAR(10)」；PostgreSQL 是区分重载用的参数表 */
  args?: string
  returns?: string
  language?: string
  created?: string
  updated?: string
  comment?: string
  oid?: number
}
/** 这台电脑上保存的查询或备份文件 */
export interface DbFileItem { name: string; path: string; size: number; modified: string; preview?: string }

/** 「对象」列表显示什么：all = 库（模式）里的表和视图 */
export type DbObjWhat = 'all' | 'table' | 'view' | 'func' | 'query' | 'backup'
/** 「对象」列表里的一行 */
export interface DbObjItem {
  kind: 'table' | 'view' | 'func' | 'query' | 'backup'
  key: string
  name: string
  t?: DbTable
  r?: DbRoutine
  f?: DbFileItem
}

/** 运行函数、存储过程的语句（在查询窗口里打开，参数让用户填） */
export function routineCallSql(kind: string, r: DbRoutine): string {
  const q = (s: string) => (kind === 'mysql' ? '`' + s.replace(/`/g, '``') + '`' : '"' + s.replace(/"/g, '""') + '"')
  const name = kind === 'postgres' && r.schema && r.schema !== 'public' ? `${q(r.schema)}.${q(r.name)}` : q(r.name)
  const hint = r.args ? `/* ${r.args.replace(/\*\//g, '* /')} */` : ''
  if (r.type === 'procedure') return `CALL ${name}(${hint});\n`
  const ret = (r.returns ?? '').toUpperCase()
  if (kind === 'postgres' && (ret.startsWith('SETOF ') || ret.startsWith('TABLE('))) return `SELECT * FROM ${name}(${hint});\n`
  return `SELECT ${name}(${hint});\n`
}

/** 从 CREATE FUNCTION / PROCEDURE 语句里取出类型、模式、名称 */
export function parseRoutineHead(sql: string): { type: 'function' | 'procedure'; schema?: string; name: string } | null {
  const m = /CREATE\s+(?:OR\s+REPLACE\s+)?(?:DEFINER\s*=\s*\S+\s+)?(FUNCTION|PROCEDURE)\s+((?:[`"][^`"]+[`"]|[\w$]+)(?:\s*\.\s*(?:[`"][^`"]+[`"]|[\w$]+))?)/i.exec(sql)
  if (!m) return null
  const parts = m[2].split(/\s*\.\s*/).map((x) => x.replace(/^[`"]|[`"]$/g, ''))
  return { type: m[1].toLowerCase() as 'function' | 'procedure', schema: parts.length > 1 ? parts[0] : undefined, name: parts[parts.length - 1] }
}

export interface DbColumnInfo {
  name: string
  type: string
  nullable: boolean
  default: string | null
  auto?: boolean
  extra?: string
  generated?: boolean
  collation?: string
  comment?: string
  key?: boolean
}
export interface DbIndexInfo { name: string; columns: string[]; unique?: boolean; primary?: boolean; method?: string; def?: string }
export interface DbTableInfo {
  schema?: string
  name: string
  type: 'table' | 'view'
  columns: DbColumnInfo[]
  indexes: DbIndexInfo[]
  foreign: { name: string; def: string }[]
  primaryKey: string[]
  key: string[]
  comment?: string
  engine?: string
  collation?: string
  charset?: string
  pkName?: string
  ddl: string
}

/** 单元格：null、文本、二进制、被截断的长文本 */
export type DbCell = string | null | { bin: number; hex: string } | { cut: string; size: number }
export interface DbColumn { name: string; type: string }
export interface DbResult {
  sql: string
  columns?: DbColumn[]
  rows?: DbCell[][]
  more?: boolean
  affected?: number
  lastId?: number
  ms: number
  error?: string
  code?: string
}

export interface DbChange {
  op: 'update' | 'insert' | 'delete'
  key?: Record<string, string | null>
  values?: Record<string, string | null>
}

export interface DbColumnDef {
  name: string
  orig?: string
  type: string
  nullable: boolean
  default: string | null
  auto?: boolean
  extra?: string
  collation?: string
  comment?: string
}
export interface DbIndexDef { name: string; orig?: string; columns: string[]; unique?: boolean; method?: string; def?: string }
export interface DbDesign {
  schema?: string
  name: string
  orig?: string
  columns: DbColumnDef[]
  primaryKey: string[]
  indexes: DbIndexDef[]
  comment?: string
  engine?: string
  charset?: string
  collation?: string
}

/** 筛选条件（dbx.Filter） */
export interface DbFilter {
  col: string
  op: string
  value?: string
  value2?: string
  /** 和前一个条件是「或」 */
  or?: boolean
  /** 暂时不用 */
  off?: boolean
}
export interface DbSort { col: string; desc?: boolean }

/** 筛选运算：值、label；noValue 不用填值，two 要填两个值 */
export const filterOps: { value: string; label: string; noValue?: boolean; two?: boolean; hint?: string }[] = [
  { value: '=', label: '等于' },
  { value: '<>', label: '不等于' },
  { value: '<', label: '小于' },
  { value: '<=', label: '小于等于' },
  { value: '>', label: '大于' },
  { value: '>=', label: '大于等于' },
  { value: 'contains', label: '包含' },
  { value: 'not-contains', label: '不包含' },
  { value: 'starts', label: '开头是' },
  { value: 'ends', label: '结尾是' },
  { value: 'null', label: '是 NULL', noValue: true },
  { value: 'not-null', label: '不是 NULL', noValue: true },
  { value: 'empty', label: '是空的（NULL 或空字符串）', noValue: true },
  { value: 'not-empty', label: '不是空的', noValue: true },
  { value: 'in', label: '在列表中', hint: '多个值用逗号隔开' },
  { value: 'not-in', label: '不在列表中', hint: '多个值用逗号隔开' },
  { value: 'between', label: '介于', two: true },
  { value: 'not-between', label: '不介于', two: true },
]

export interface DbReq {
  schema?: string
  table?: string
  where?: string
  filters?: DbFilter[]
  sorts?: DbSort[]
  tables?: string[]
  data?: boolean
  /** tables：多读自增值、修改时间、数据和索引大小（「对象」列表） */
  detail?: boolean
  /** 函数、存储过程（routine、routine-save；新建时为空） */
  routine?: { schema?: string; name: string; type: string; args?: string; oid?: number } | null
  /** 保存的查询：名称、原来的名称、覆盖同名的；备份的名称 */
  name?: string
  old?: string
  overwrite?: boolean
  offset?: number
  limit?: number
  session?: string
  sql?: string
  stmts?: string[]
  changes?: DbChange[]
  design?: DbDesign
  op?: string
  args?: string[]
  format?: string
  out?: string
  preview?: boolean
  max?: number
  stop?: boolean
  // Redis
  scan?: { pattern?: string; type?: string; cursor?: string; count?: number }
  get?: { key: RStr; offset?: number; limit?: number; cursor?: string; match?: string; desc?: boolean }
  edit?: RedisEdit
  line?: string
  confirm?: boolean
}

// ---------------------------------------------------------------- Redis（dbx/redis.go）

/** 界面传给 ferry 的键、字段、成员：文字，二进制时带 base64 */
export interface RStr { t: string; b64?: string }
/** 显示用的值：二进制时 t 是转义过的（bin 为 true）；size 不为 0 时被截断了，原来有 size 字节 */
export interface RedisVal { t: string; b64?: string; bin?: boolean; size?: number }
export interface RedisKey { name: string; b64?: string; type: string }
export interface RedisKeys { keys: RedisKey[]; cursor: string; scanned: number; total: number; ms: number }
/** 值里的一项：列表 i、v；哈希 f、v；集合 f；有序集合 f、s（分数）；流 f（ID）、p（字段、值交替） */
export interface RedisItem { i?: number; f?: RedisVal; v?: RedisVal; s?: string; p?: RedisVal[] }
export interface RedisValue {
  key: RedisKey
  exists: boolean
  type: string
  /** 毫秒；-1 永不过期 */
  ttl: number
  size: number
  memory?: number
  encoding?: string
  value?: RedisVal
  items?: RedisItem[]
  offset?: number
  cursor?: string
  more?: boolean
  ms: number
}
export interface RedisEdit {
  op: string
  key?: RStr
  keys?: RStr[]
  type?: string
  field?: RStr
  orig?: RStr
  value?: string
  index?: number
  score?: string
  pairs?: string[]
  head?: boolean
  ttl?: number
  newKey?: RStr
}
export interface RedisReply { t: 'str' | 'int' | 'nil' | 'err' | 'arr'; v?: string; bin?: boolean; items?: RedisReply[]; more?: number; cut?: boolean }
export interface RedisCmdResult { reply?: RedisReply; db: number; ms: number; confirm?: string }

export const redisTypes: Record<string, string> = { string: '字符串', hash: '哈希', list: '列表', set: '集合', zset: '有序集合', stream: '流' }

/** 键、字段、成员转成传给 ferry 的样子（二进制的带 base64） */
export function rstr(v: { name?: string; t?: string; b64?: string }): RStr {
  const t = v.name ?? v.t ?? ''
  return v.b64 ? { t, b64: v.b64 } : { t }
}

/** 能在界面上改（不是二进制、没被截断） */
export function editable(v?: RedisVal): boolean {
  return !!v && !v.bin && !v.size
}

/** 剩下的过期时间说成人话 */
export function ttlText(ms: number): string {
  if (ms === -1) return '永不过期'
  if (ms < 0) return '已过期'
  const s = Math.round(ms / 1000)
  if (s < 60) return `${s} 秒后过期`
  const d = Math.floor(s / 86400)
  const h = Math.floor((s % 86400) / 3600)
  const m = Math.floor((s % 3600) / 60)
  const parts: string[] = []
  if (d) parts.push(`${d} 天`)
  if (h) parts.push(`${h} 小时`)
  if (m && !d) parts.push(`${m} 分`)
  return `${parts.join(' ')}后过期`
}

/** 像 redis-cli 那样把回复排成几行 */
export function formatReply(r: RedisReply, indent = ''): string[] {
  switch (r.t) {
    case 'nil':
      return ['(nil)']
    case 'int':
      return [`(integer) ${r.v}`]
    case 'err':
      return [`(error) ${r.v}`]
    case 'str': {
      const v = r.v ?? ''
      const cut = r.cut ? ' …（太长，只显示了前面一部分）' : ''
      // 多行的文字（INFO、CLIENT LIST……）原样显示；其余像 redis-cli 一样加引号
      if (!r.bin && v.includes('\n')) return (v.replace(/\r\n/g, '\n').replace(/\n$/, '') + cut).split('\n')
      // 状态回复（OK、PONG……）redis-cli 不加引号；go-redis 分不出状态和字符串，按常见的几个认
      if (!r.bin && (v === 'OK' || v === 'PONG' || v === 'QUEUED')) return [v]
      const q = r.bin ? v : v.replace(/\\/g, '\\\\').replace(/"/g, '\\"').replace(/\t/g, '\\t').replace(/\r/g, '\\r')
      return [`"${q}"${cut}`]
    }
    case 'arr': {
      const items = r.items ?? []
      if (!items.length && !r.more) return ['(empty array)']
      const out: string[] = []
      const w = String(items.length).length
      items.forEach((it, i) => {
        const head = `${String(i + 1).padStart(w)}) `
        const pad = ' '.repeat(head.length)
        formatReply(it, indent + pad).forEach((l, j) => out.push(j === 0 ? head + l : pad + l))
      })
      if (r.more) out.push(`…（还有 ${r.more} 项没显示）`)
      return out
    }
  }
  return []
}

export const kindLabel: Record<DbKind, string> = { mysql: 'MySQL / MariaDB', postgres: 'PostgreSQL', redis: 'Redis' }
export const defaultPort: Record<DbKind, number> = { mysql: 3306, postgres: 5432, redis: 6379 }
export const defaultUser: Record<DbKind, string> = { mysql: 'root', postgres: 'postgres', redis: '' }

/** 与 dbx.PasswordEnv 一致 */
export function passwordEnv(id: string): string {
  return 'FERRY_DB_PASSWORD_' + id.toUpperCase().replace(/[^A-Z0-9]/g, '_')
}

export const dbOpen = ref(false)
export const dbConns = ref<DbConn[]>([])
/** 打开着的连接（已经连上过） */
export const dbOpened = reactive<Record<string, boolean>>({})
/** 从「软件」页连数据库：哪个软件、在哪台服务器上 */
export interface DbFrom {
  sw: Software
  via: DbVia
  /** 服务器地址（直接连时用） */
  host: string
  endpoint: Endpoint
}
/** 从「软件」页点「连接数据库」：打开抽屉并新建（或选中已有的）连接 */
export const dbRequest = ref<(Partial<DbConn> & { from?: DbFrom }) | null>(null)

/** 「软件」页的一行 → 新建连接的请求：默认用服务器地址直接连（直接连不上的经服务器连），对话框里可以改 */
export function dbRequestFromSoftware(sw: Software, via: DbVia, host: string): (Partial<DbConn> & { from: DbFrom }) | null {
  const c = connFromSoftware(sw, via, via.title || via.target)
  if (!c?.kind) return null
  const endpoint = dbEndpoint(sw, defaultPort[c.kind])
  return { ...c, from: { sw, via, host, endpoint } }
}

/** 从服务器上读到的数据库账号密码（engine.DBCred） */
/** found：有可以填的密码；verified：经服务器登录试过，能登录 */
export interface DbCred { user?: string; password?: string; found: boolean; verified?: boolean; from?: string; note?: string }

/** 从服务器上读数据库的账号密码（容器的环境变量、redis.conf、~/.my.cnf……；Ferry 装的用安装时保存的） */
export async function dbCreds(from: DbFrom): Promise<DbCred> {
  const { sw, via, endpoint } = from
  // 容器里的端口：试密码时直接连容器（端口映射到主机的也一样）
  const inner = (sw.inner ?? []).map((l) => Number(l.slice(l.lastIndexOf(':') + 1))).filter((p) => p > 0)
  const def = sw.kind ? defaultPort[sw.kind === 'postgresql' ? 'postgres' : sw.kind === 'redis' ? 'redis' : 'mysql'] : 0
  // 不知道容器里的端口时（只读到了映射到主机的端口）按默认端口试：官方镜像都用默认端口
  const innerPort = sw.via === 'docker' && sw.container ? (inner.includes(def) ? def : inner[0] ?? def) : undefined
  const cred = { kind: sw.kind, container: sw.via === 'docker' ? sw.container : undefined, unit: sw.unit, pid: sw.pid,
    port: endpoint.ssh.container ? undefined : endpoint.ssh.port, innerPort: innerPort ?? (endpoint.ssh.container ? endpoint.ssh.port : undefined),
    instance: sw.instance, account: sw.account }
  return call<DbCred>(['db', 'creds'], { FERRY_DB_REQ: JSON.stringify({ via: { config: via.config, target: via.target }, cred }) })
}

/** 要用户填数据库密码 */
export const dbPasswordRequest = ref<{ conn: DbConn; rejected: boolean; resolve: (ok: boolean) => void } | null>(null)

export async function loadDbConns() {
  const r = await call<{ connections: DbConn[]; opened: string[] }>(['db', 'list'])
  dbConns.value = r.connections
  for (const k of Object.keys(dbOpened)) delete dbOpened[k]
  for (const id of r.opened) dbOpened[id] = true
}

function reqEnv(req: DbReq, extra: Record<string, string> = {}): Record<string, string> {
  return { FERRY_DB_REQ: JSON.stringify(req), ...extra }
}

/** 弹出主机公钥核对、SSH 密码框：处理好了（retry 被调用）为 true；框都关了也没处理为 false。 */
function ask<T extends { retry?: () => void }>(r: Ref<T | null>, item: T): Promise<boolean> {
  return new Promise((resolve) => {
    let settled = false
    const stop = watch(r, (v) => {
      if (v === null) setTimeout(() => { if (!settled) { settled = true; stop(); resolve(false) } }, 300)
    })
    item.retry = () => { if (!settled) { settled = true; stop(); resolve(true) } }
    r.value = item
  })
}

/** 连接问题（主机公钥、SSH 密码、数据库密码）先请用户处理，处理好了再试一次。 */
async function withRetry<T>(conn: DbConn | undefined, run: () => Promise<T>, tries = 3): Promise<T> {
  for (let i = 0; ; i++) {
    try {
      return await run()
    } catch (e) {
      if (!(e instanceof FerryError) || i >= tries) throw e
      const d = e.detail
      if (conn && (d?.kind === 'db-password-required' || d?.kind === 'db-password-rejected')) {
        const ok = await new Promise<boolean>((resolve) => { dbPasswordRequest.value = { conn, rejected: d.kind === 'db-password-rejected', resolve } })
        if (!ok) throw new FerryError('没有填密码', d)
        continue
      }
      const via = conn?.via
      if (via && e.unknownHost) {
        const ok = await ask(trustRequest, { target: via.target, config: via.config })
        if (!ok) throw e
        continue
      }
      if (via && d && isSecretKind(d.kind)) {
        const ok = await ask(secretRequest as Ref<{ detail: ErrorDetail; config: string; retry?: () => void } | null>, { detail: d as ErrorDetail, config: via.config })
        if (!ok) throw e
        continue
      }
      throw e
    }
  }
}

/** 调用 ferry db <sub> <连接> [库]，自动处理要密码、核对主机公钥的情况。 */
export async function dbCall<T>(conn: DbConn, sub: string, db: string | undefined, req: DbReq = {}): Promise<T> {
  const r = await withRetry(conn, () => call<T>(['db', sub, conn.id, ...(db !== undefined ? [db] : [])], reqEnv(req)))
  dbOpened[conn.id] = true
  return r
}

/** 能取消的调用（查询、导出）。 */
export function dbRun<T>(conn: DbConn, sub: string, db: string | undefined, req: DbReq, onInfo?: (msg: string) => void): { done: Promise<T>; cancel: () => void } {
  let current: { kill: () => Promise<void> } | null = null
  let canceled = false
  const done = withRetry(conn, async () => {
    if (canceled) throw new FerryError('已取消')
    const p = ferry<T>(['db', sub, conn.id, ...(db !== undefined ? [db] : [])], (l) => l.kind === 'info' && l.msg && onInfo?.(l.msg), reqEnv(req))
    current = p
    const r = await p.done
    if (!r.ok) throw new FerryError(r.error ?? '未知错误', r.detail)
    return r.data as T
  }).then((r) => { dbOpened[conn.id] = true; return r })
  return { done, cancel: () => { canceled = true; void current?.kill() } }
}

/** 保存连接（新建时 id 为空）。password 为 undefined 表示不改密码。 */
export async function saveDbConn(c: Partial<DbConn>, password?: string, aiPassword?: string): Promise<DbConn> {
  const env: Record<string, string> = { FERRY_DB_CONN: JSON.stringify(c) }
  if (password) env.FERRY_DB_NEW_PASSWORD = password
  if (aiPassword) env.FERRY_DB_NEW_AI_PASSWORD = aiPassword
  const saved = await withRetry(undefined, () => call<DbConn>(['db', 'save'], env))
  if (password && !saved.savePassword) sessionEnv[passwordEnv(saved.id)] = password
  if (!saved.savePassword && !password) delete sessionEnv[passwordEnv(saved.id)]
  delete dbOpened[saved.id]
  await loadDbConns()
  return saved
}

/** 测试连接（不保存），返回版本和库。 */
export async function testDbConn(c: Partial<DbConn>, password?: string, aiPassword?: string): Promise<{ version: string; databases: DbDatabase[]; encrypted?: boolean; viaSSH?: boolean }> {
  const env: Record<string, string> = { FERRY_DB_CONN: JSON.stringify(c) }
  if (aiPassword) env.FERRY_DB_NEW_AI_PASSWORD = aiPassword
  if (password) env.FERRY_DB_NEW_PASSWORD = password
  else if (c.id && sessionEnv[passwordEnv(c.id)]) env.FERRY_DB_NEW_PASSWORD = sessionEnv[passwordEnv(c.id)]
  return withRetry({ ...(c as DbConn) }, () => call(['db', 'test'], env), 2)
}

export async function removeDbConn(id: string) {
  await call(['db', 'remove', id])
  delete sessionEnv[passwordEnv(id)]
  await loadDbConns()
}

export async function closeDbConn(id: string) {
  await call(['db', 'close', id])
  delete dbOpened[id]
}

export function closeDbSession(id: string, session: string) {
  void call(['db', 'close', id], reqEnv({ session })).catch(() => {})
}

/** 软件页的一项 → 新连接的默认值（经这台服务器连）。 */
export function connFromSoftware(sw: Software, via: DbVia, server: string): Partial<DbConn> | null {
  const kind: DbKind | null = sw.kind === 'mysql' || sw.kind === 'mariadb' ? 'mysql' : sw.kind === 'postgresql' ? 'postgres' : sw.kind === 'redis' ? 'redis' : null
  if (!kind) return null
  const c: Partial<DbConn> = { kind, via, user: defaultUser[kind], name: `${sw.name} · ${server}`, savePassword: true }
  const l = sw.listen?.[0]
  if (l) {
    const i = l.lastIndexOf(':')
    const host = l.slice(0, i).replace(/^\[|\]$/g, '')
    c.port = Number(l.slice(i + 1)) || defaultPort[kind]
    c.host = !host || host === '0.0.0.0' || host === '::' || host === '*' ? '127.0.0.1' : host
  } else if (sw.container && sw.inner?.length) {
    const l2 = sw.inner[0]
    c.container = sw.container
    c.port = Number(l2.slice(l2.lastIndexOf(':') + 1)) || defaultPort[kind]
    c.host = ''
  } else {
    c.host = '127.0.0.1'
    c.port = defaultPort[kind]
  }
  if (sw.instance) {
    c.software = sw.instance
    if (sw.account && kind !== 'redis') c.user = sw.account
  }
  return c
}

export function isDbSoftware(sw: Software): boolean {
  return sw.kind === 'mysql' || sw.kind === 'mariadb' || sw.kind === 'postgresql' || sw.kind === 'redis'
}

/** 单元格的文字（显示、复制用） */
export function cellText(v: DbCell): string {
  if (v === null) return 'NULL'
  if (typeof v === 'string') return v
  if ('bin' in v) return `(二进制 ${v.bin} 字节) 0x${v.hex}${v.bin > v.hex.length / 2 ? '…' : ''}`
  return v.cut + '…'
}

export function humanSize(n?: number): string {
  if (n === undefined || n === null) return ''
  if (n < 1024) return `${n} B`
  if (n < 1024 ** 2) return `${(n / 1024).toFixed(1)} KB`
  if (n < 1024 ** 3) return `${(n / 1024 ** 2).toFixed(1)} MB`
  return `${(n / 1024 ** 3).toFixed(2)} GB`
}

/** 粗看一条语句有没有危险（不带 WHERE 的 UPDATE / DELETE、DROP、TRUNCATE）：执行前多问一句 */
/** 这些语句危险在哪（删表、清空、不带 WHERE 的删改、删列）：交给 ferry 按数据库的语法拆语句、判断，和 AI 改动的确认框同一套规则。 */
export async function sqlDangers(conn: DbConn, what: { sql?: string; stmts?: string[] }): Promise<string[]> {
  return (await sqlCheck(conn, what)).dangers
}

/** 危险在哪，和按语法拆开后一共几条语句。 */
export async function sqlCheck(conn: DbConn, what: { sql?: string; stmts?: string[] }): Promise<{ dangers: string[]; count: number }> {
  const r = await call<{ dangers: string[]; count: number }>(['db', 'danger', conn.id], reqEnv(what))
  return { dangers: r.dangers ?? [], count: r.count }
}

/** 执行改动前给用户看要执行的语句 */
export const sqlConfirm = ref<{ title: string; conn: DbConn; stmts: string[]; note?: string; okText?: string; whole?: boolean; resolve: (ok: boolean) => void } | null>(null)

/** whole：每条都是完整的一条（建函数、存储过程，里面有分号），不再按分号拆开数、也不标危险 */
export function confirmSql(conn: DbConn, title: string, stmts: string[], opts: { note?: string; okText?: string; whole?: boolean } = {}): Promise<boolean> {
  return new Promise((resolve) => { sqlConfirm.value = { title, conn, stmts, ...opts, resolve } })
}

/**
 * 右键菜单：在菜单外按下鼠标（含右键另一处）、按 Esc、滚动、切到别的窗口时关闭。
 * 用捕获阶段监听：抽屉、对话框会拦住冒泡的 click，冒泡阶段收不到。返回取消监听的函数。
 */
export function closeOnOutside(inside: (t: Node) => boolean, close: () => void): () => void {
  const done = () => { off(); close() }
  const down = (e: Event) => { if (!inside(e.target as Node)) done() }
  const key = (e: KeyboardEvent) => { if (e.key === 'Escape') { e.stopPropagation(); done() } }
  const wheel = (e: Event) => { if (!inside(e.target as Node)) done() }
  function off() {
    document.removeEventListener('pointerdown', down, true)
    document.removeEventListener('keydown', key, true)
    document.removeEventListener('wheel', wheel, true)
    window.removeEventListener('blur', done)
    window.removeEventListener('resize', done)
  }
  document.addEventListener('pointerdown', down, true)
  document.addEventListener('keydown', key, true)
  document.addEventListener('wheel', wheel, true)
  window.addEventListener('blur', done)
  window.addEventListener('resize', done)
  return off
}

/** 导出一张表（或整个库 / 模式：table 为空）：csv、insert 只导出数据（按当前的筛选、排序），struct 只导出结构，dump 导出结构和数据 */
export type ExportKind = 'csv' | 'insert' | 'struct' | 'dump'
export const exportLabels: Record<ExportKind, string> = {
  csv: '导出数据为 CSV（Excel 能打开）',
  insert: '导出数据为 SQL（INSERT 语句）',
  struct: '导出结构（建表语句）',
  dump: '导出结构和数据',
}
export async function exportTo(conn: DbConn, db: string, what: { schema?: string; table?: string }, kind: ExportKind, browse: Pick<DbReq, 'where' | 'filters' | 'sorts'> = {}) {
  const { pickSaveFile, reveal } = await import('./platform')
  const { startTask } = await import('./state')
  const { ElMessage } = await import('element-plus')
  const base = what.table || (what.schema && what.schema !== 'public' ? `${db}.${what.schema}` : db) || conn.name
  const ext = kind === 'csv' ? 'csv' : 'sql'
  const suffix = kind === 'struct' ? '_结构' : kind === 'dump' ? '_结构和数据' : ''
  const out = await pickSaveFile(`导出 ${base}`, `${base}${suffix}.${ext}`, ext === 'csv' ? 'CSV 表格' : 'SQL 文件', [ext])
  if (!out) return
  const sub = kind === 'struct' || kind === 'dump' ? 'dump' : 'export'
  const req: DbReq = sub === 'dump'
    ? { schema: what.schema, tables: what.table ? [what.table] : [], data: kind === 'dump', out }
    : { schema: what.schema, table: what.table, format: kind === 'csv' ? 'csv' : 'sql', out, ...browse }
  const title = `${kind === 'struct' ? '导出结构' : kind === 'dump' ? '导出结构和数据' : '导出数据'}：${conn.name} / ${db ? db + ' / ' : ''}${what.table ?? what.schema ?? ''}`
  startTask(title, ['db', sub, conn.id, db], (t) => {
    if (t.status === 'ok') {
      ElMessage.success({ message: `已导出到 ${out}`, duration: 4000 })
      void reveal(out)
    }
  }, { FERRY_DB_REQ: JSON.stringify(req) })
}
