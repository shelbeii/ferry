// 运行环境适配：Tauri 桌面版用 sidecar / 插件；在普通浏览器里调试时走开发桥接（scripts/dev-bridge.mjs）。
import { Command } from '@tauri-apps/plugin-shell'
import { invoke } from '@tauri-apps/api/core'
import { open, save } from '@tauri-apps/plugin-dialog'
import { LazyStore } from '@tauri-apps/plugin-store'

declare const __BRIDGE_TOKEN__: string
declare const __APP_VERSION__: string

export const isTauri = typeof window !== 'undefined' && '__TAURI_INTERNALS__' in window
const BRIDGE = 'http://127.0.0.1:1421'

export interface Proc {
  /** 命令结束，返回退出码 */
  done: Promise<number | null>
  kill: () => Promise<void>
  /** 终端请求的其他操作：input（键盘输入）、resize（窗口大小）、flow（暂停 / 继续输出） */
  post: (op: 'input' | 'resize' | 'flow', fields: Record<string, unknown>) => void
}

/**
 * 在常驻的 ferry serve 里执行一条 ferry 命令，stdout / stderr 逐行回调。
 * 所有操作共用一个后台进程，到服务器的连接在操作之间复用（像终端工具的会话一样），空闲一段时间后自动断开。
 * env：本次命令额外的环境变量（密码等只经环境变量传递，不出现在命令行参数里）。
 * onTty：终端（ferry terminal）的输出，原样的一段文字。
 */
export function spawnFerry(
  args: string[],
  onOut: (line: string) => void,
  onErr: (line: string) => void,
  env: Record<string, string> = {},
  onTty?: (data: string) => void,
): Proc {
  const id = page + ++seq
  let resolve!: (c: number | null) => void
  const done = new Promise<number | null>((r) => (resolve = r))
  const srv = isTauri ? tauriServer() : bridgeServer()
  pending.set(id, { onOut, onErr, onTty, resolve, srv })
  srv.send({ id, op: 'run', args, env }).catch((e) => {
    if (pending.delete(id)) {
      onErr(String(e))
      resolve(-1)
    }
  })
  return {
    done,
    kill: async () => {
      if (!pending.has(id)) return
      await srv.send({ id, op: 'cancel' }).catch(() => undefined)
      // 等它收尾（关闭这次操作的远程命令、上传或构建），最多 5 秒
      await Promise.race([done, new Promise((r) => setTimeout(r, 5000))])
    },
    post: (op, fields) => {
      if (pending.has(id)) srv.send({ ...fields, id, op }).catch(() => undefined)
    },
  }
}

interface Server {
  send: (msg: unknown) => Promise<void>
}

interface Pending {
  onOut: (l: string) => void
  onErr: (l: string) => void
  onTty?: (data: string) => void
  resolve: (c: number | null) => void
  srv: Server
}

let seq = 0
/** 每个页面的请求编号前缀（浏览器调试时多个页面共用一个 ferry serve） */
const page = Math.random().toString(36).slice(2, 8) + '-'
const pending = new Map<string, Pending>()

/** 处理 ferry serve 的一行输出：{id, outs} / {id, out} / {id, err} / {id, tty} / {id, exit}。 */
function dispatch(raw: string) {
  const line = raw.replace(/\r?\n$/, '')
  if (!line) return
  let m: { id?: string; out?: string; outs?: string[]; err?: string; tty?: string; exit?: number }
  try {
    m = JSON.parse(line)
  } catch {
    console.warn('[ferry serve]', line)
    return
  }
  const p = m.id ? pending.get(m.id) : undefined
  if (!p) return
  if (m.out !== undefined) p.onOut(m.out)
  // stdout 一批几行（日志刷得快时 ferry serve 攒一会儿一起发）
  if (m.outs) for (const l of m.outs) p.onOut(l)
  if (m.err !== undefined) p.onErr(m.err)
  if (m.tty !== undefined) p.onTty?.(m.tty)
  if (m.exit !== undefined) {
    pending.delete(m.id!)
    p.resolve(m.exit)
  }
}

/** 后台进程没了：交给它的操作都按失败结束，下次操作时重新启动。 */
function lost(srv: Server, why: string) {
  for (const [id, p] of pending) {
    if (p.srv !== srv) continue
    pending.delete(id)
    p.onErr(why)
    p.resolve(-1)
  }
}

/** 一个接一个地发：终端的键盘输入不能乱序（Tauri 的异步命令、同时发出的 fetch 都不保证先后） */
function inOrder(send: (msg: unknown) => Promise<void>): (msg: unknown) => Promise<void> {
  let last: Promise<unknown> = Promise.resolve()
  return (msg) => {
    const p = last.then(() => send(msg))
    last = p.catch(() => undefined)
    return p
  }
}

let tauri: Server | undefined

function tauriServer(): Server {
  if (tauri) return tauri
  const cmd = Command.sidecar('binaries/ferry', ['serve'])
  const child = cmd.spawn()
  const srv: Server = {
    send: inOrder(async (msg) => {
      const c = await child
      await c.write(JSON.stringify(msg) + '\n')
    }),
  }
  const gone = (why: string) => {
    if (tauri === srv) tauri = undefined
    lost(srv, why)
  }
  cmd.stdout.on('data', dispatch)
  cmd.stderr.on('data', (l: string) => console.warn('[ferry serve]', l))
  cmd.on('close', (p) => gone(`ferry 后台进程退出（退出码 ${p.code}），进行中的操作已中断，下次操作会自动重新启动`))
  cmd.on('error', (e) => gone(`ferry 后台进程出错：${e}`))
  child.catch((e) => gone(`无法启动 ferry：${e}`))
  tauri = srv
  return srv
}

// 浏览器调试：开发桥接里同样只有一个 ferry serve，这里用一条长连接接收它的全部输出
let bridgeSrv: Server | undefined

function bridgeServer(): Server {
  if (bridgeSrv) return bridgeSrv
  const srv: Server = {
    send: inOrder(async (msg) => {
      await ready
      await bridge('/send', msg)
    }),
  }
  let opened!: () => void
  const ready = new Promise<void>((r) => (opened = r))
  ;(async () => {
    try {
      const res = await bridge('/events', {})
      opened()
      const reader = res.body!.pipeThrough(new TextDecoderStream()).getReader()
      let buf = ''
      for (;;) {
        const { value, done } = await reader.read()
        if (done) break
        buf += value
        let i
        while ((i = buf.indexOf('\n')) >= 0) {
          dispatch(buf.slice(0, i))
          buf = buf.slice(i + 1)
        }
      }
      if (bridgeSrv === srv) bridgeSrv = undefined
      lost(srv, '开发桥接断开')
    } catch (e) {
      if (bridgeSrv === srv) bridgeSrv = undefined
      opened()
      lost(srv, `开发桥接不可用：${e}（先运行 node scripts/dev-bridge.mjs）`)
    }
  })()
  bridgeSrv = srv
  return srv
}

async function bridge(path: string, body: unknown): Promise<Response> {
  const res = await fetch(BRIDGE + path, {
    method: 'POST',
    headers: { 'content-type': 'application/json', 'x-bridge-token': __BRIDGE_TOKEN__ },
    body: JSON.stringify(body),
  })
  if (!res.ok) throw new Error(await res.text())
  return res
}

// ---------------------------------------------------------------- 配置文件

export async function readConfig(path: string): Promise<string> {
  if (isTauri) return invoke<string>('read_config', { path })
  return (await bridge('/read_config', { path })).json()
}

export async function writeConfig(path: string, content: string): Promise<void> {
  if (isTauri) return invoke('write_config', { path, content })
  await bridge('/write_config', { path, content })
}

/** 选择 ferry.yaml（浏览器调试时手工输入路径）。 */
export async function pickConfig(): Promise<string | null> {
  if (!isTauri) return promptPath('ferry.yaml 的绝对路径')
  const picked = await open({
    title: '选择 ferry.yaml',
    multiple: false,
    directory: false,
    filters: [{ name: 'ferry 配置', extensions: ['yaml', 'yml'] }],
  })
  return picked ? String(picked) : null
}

// ---------------------------------------------------------------- 设置存储

const store = isTauri ? new LazyStore('projects.json') : null

export async function storeGet<T>(key: string): Promise<T | undefined> {
  if (store) return store.get<T>(key)
  const v = localStorage.getItem('ferry:' + key)
  return v === null ? undefined : (JSON.parse(v) as T)
}

export async function storeSet(key: string, value: unknown): Promise<void> {
  if (store) {
    await store.set(key, value)
    await store.save()
    return
  }
  localStorage.setItem('ferry:' + key, JSON.stringify(value))
}

// ---------------------------------------------------------------- 其他

/** 浏览器调试时也用应用内的输入弹窗，和桌面版保持相同的主题与排版。 */
async function promptPath(title: string, value = ''): Promise<string | null> {
  const { ElMessageBox } = await import('element-plus')
  try {
    const result = await ElMessageBox.prompt('请输入本机上的路径。', title, { inputValue: value, confirmButtonText: '确定', cancelButtonText: '取消' })
    return result.value
  } catch {
    return null
  }
}

/** 在资源管理器 / Finder 中显示（浏览器调试时不可用）。 */
export async function reveal(path: string): Promise<boolean> {
  if (!isTauri) return false
  const { revealItemInDir } = await import('@tauri-apps/plugin-opener')
  await revealItemInDir(path)
  return true
}

/** 选择文件夹（浏览器调试时手工输入）。 */
/** 选择一个文件（浏览器调试时手工输入路径）；extensions 为空 = 不限类型（私钥这类没有扩展名的文件）。 */
export async function pickFile(title: string, name: string, extensions: string[]): Promise<string | null> {
  if (!isTauri) return promptPath(title)
  const picked = await open({ title, multiple: false, directory: false, filters: extensions.length ? [{ name, extensions }] : undefined })
  return picked ? String(picked) : null
}

/** 选择保存到哪里（浏览器调试时手工输入路径）。 */
export async function pickSaveFile(title: string, defaultName: string, name: string, extensions: string[]): Promise<string | null> {
  if (!isTauri) return promptPath(title, defaultName)
  const picked = await save({ title, defaultPath: defaultName, filters: [{ name, extensions }] })
  return picked ? String(picked) : null
}

export async function pickDir(title: string): Promise<string | null> {
  if (!isTauri) return promptPath(title)
  const picked = await open({ title, directory: true, multiple: false })
  return picked ? String(picked) : null
}

/** 选择多个文件或文件夹（浏览器调试时手工输入，多个用 | 分开）。 */
export async function pickMany(title: string, directory: boolean): Promise<string[]> {
  if (!isTauri) {
    const v = await promptPath(title)
    return v ? v.split('|').map((s) => s.trim()).filter(Boolean) : []
  }
  const picked = await open({ title, directory, multiple: true })
  if (!picked) return []
  return (Array.isArray(picked) ? picked : [picked]).map(String)
}

/**
 * 从资源管理器 / Finder 拖进窗口的文件：hit(x, y) 判断落点（CSS 像素）是否在接收区域内，在就回调 onDrop。
 * 返回取消监听的函数。浏览器调试时不可用。
 */
export async function onFileDrop(hit: (x: number, y: number) => boolean, onDrop: (paths: string[]) => void, onOver?: (over: boolean) => void): Promise<() => void> {
  if (!isTauri) return () => undefined
  const { getCurrentWebview } = await import('@tauri-apps/api/webview')
  return getCurrentWebview().onDragDropEvent((e) => {
    const p = e.payload
    const ratio = window.devicePixelRatio || 1
    if (p.type === 'over' || p.type === 'enter') onOver?.(hit(p.position.x / ratio, p.position.y / ratio))
    else if (p.type === 'leave') onOver?.(false)
    else if (p.type === 'drop') {
      onOver?.(false)
      if (hit(p.position.x / ratio, p.position.y / ratio) && p.paths.length) onDrop(p.paths)
    }
  })
}

/** 桌面版自身的信息。 */
export async function appInfo(): Promise<{ name: string; version: string; tauri: string }> {
  if (!isTauri) return { name: 'Ferry', version: __APP_VERSION__, tauri: '（浏览器调试）' }
  const app = await import('@tauri-apps/api/app')
  return { name: await app.getName(), version: await app.getVersion(), tauri: await app.getTauriVersion() }
}

/** 用系统默认浏览器打开网址。 */
export async function openUrl(url: string): Promise<void> {
  if (!isTauri) {
    window.open(url, '_blank', 'noopener')
    return
  }
  const { openUrl } = await import('@tauri-apps/plugin-opener')
  await openUrl(url)
}
