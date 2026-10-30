// 开发用桥接：在普通浏览器里调试界面时，代替 Tauri 运行常驻的 ferry serve、读写配置。
// 只监听 127.0.0.1，要求随机令牌（写入 .bridge-token，由 Vite 注入页面），并且只接受来自开发页面的 Origin，
// 防止其他网页借它在本机执行命令。正式的桌面版不使用它。
//
//   node scripts/dev-bridge.mjs     然后 npm run dev，在浏览器打开 http://localhost:1420
import { createServer } from 'node:http'
import { spawn, execFileSync } from 'node:child_process'
import { randomBytes } from 'node:crypto'
import { readFileSync, writeFileSync, renameSync, existsSync } from 'node:fs'
import { dirname, isAbsolute, join, resolve, extname } from 'node:path'
import { fileURLToPath } from 'node:url'

const here = dirname(fileURLToPath(import.meta.url))
const root = resolve(here, '..')
const PORT = 1421
const ORIGIN = 'http://localhost:1420'
const token = randomBytes(24).toString('hex')
writeFileSync(join(root, '.bridge-token'), token)

const triple = /host: (\S+)/.exec(execFileSync('rustc', ['-vV'], { encoding: 'utf8' }))?.[1]
const exe = join(root, 'src-tauri', 'binaries', `ferry-${triple}${triple?.includes('windows') ? '.exe' : ''}`)
if (!existsSync(exe)) {
  console.error(`没有 sidecar：${exe}（先 npm run sidecar）`)
  process.exit(1)
}

// 与桌面版一样只有一个常驻的 ferry serve：页面通过 /events 收它的输出，通过 /send 发请求
let serve = null
const listeners = new Set()

function startServe() {
  if (serve) return serve
  const child = spawn(exe, ['serve'], { windowsHide: true, env: process.env })
  let buf = ''
  child.stdout.setEncoding('utf8')
  child.stdout.on('data', (d) => {
    buf += d
    let i
    while ((i = buf.indexOf('\n')) >= 0) {
      const line = buf.slice(0, i).replace(/\r$/, '')
      buf = buf.slice(i + 1)
      for (const res of listeners) res.write(line + '\n')
    }
  })
  child.stderr.on('data', (d) => process.stderr.write(d))
  child.on('close', (code) => {
    console.log(`ferry serve 退出（${code}）`)
    if (serve === child) serve = null
    for (const res of listeners) res.end()
    listeners.clear()
  })
  serve = child
  return child
}

function configPath(p) {
  if (typeof p !== 'string' || !isAbsolute(p) || !['.yaml', '.yml'].includes(extname(p).toLowerCase())) {
    throw new Error(`只能读写 YAML 配置文件：${p}`)
  }
  return p
}

async function body(req) {
  const chunks = []
  for await (const c of req) chunks.push(c)
  return JSON.parse(Buffer.concat(chunks).toString('utf8') || '{}')
}

createServer(async (req, res) => {
  const origin = req.headers.origin
  if (origin !== ORIGIN) {
    res.writeHead(403).end('forbidden origin')
    return
  }
  res.setHeader('Access-Control-Allow-Origin', ORIGIN)
  res.setHeader('Access-Control-Allow-Headers', 'content-type, x-bridge-token')
  if (req.method === 'OPTIONS') {
    res.writeHead(204).end()
    return
  }
  if (req.headers['x-bridge-token'] !== token) {
    res.writeHead(401).end('bad token')
    return
  }
  try {
    const url = new URL(req.url, 'http://x')
    const b = await body(req)
    if (url.pathname === '/events') {
      startServe()
      res.writeHead(200, { 'content-type': 'application/x-ndjson' })
      res.flushHeaders()
      listeners.add(res)
      req.on('close', () => listeners.delete(res))
      return
    }
    if (url.pathname === '/send') {
      if (typeof b.id !== 'string' || !['run', 'cancel', 'input', 'resize', 'flow'].includes(b.op)) throw new Error('请求格式错误')
      const msg = { id: b.id, op: b.op }
      if (b.op === 'run') {
        if (!Array.isArray(b.args) || !b.args.every((a) => typeof a === 'string')) throw new Error('args 格式错误')
        msg.args = b.args
        // 只接受 FERRY_ 开头的环境变量（密码等）
        msg.env = Object.fromEntries(Object.entries(b.env ?? {}).filter(([k, v]) => /^FERRY_[A-Z0-9_]+$/.test(k) && typeof v === 'string'))
      }
      // 终端：键盘输入、窗口大小、暂停 / 继续输出
      if (b.op === 'input') {
        if (typeof b.data === 'string') msg.data = b.data
        if (typeof b.b64 === 'string') msg.b64 = b.b64
      }
      if (b.op === 'resize') {
        msg.cols = Number(b.cols) | 0
        msg.rows = Number(b.rows) | 0
      }
      if (b.op === 'flow') msg.pause = b.pause === true
      startServe().stdin.write(JSON.stringify(msg) + '\n')
      res.writeHead(204).end()
      return
    }
    if (url.pathname === '/read_config') {
      res.writeHead(200, { 'content-type': 'application/json' }).end(JSON.stringify(readFileSync(configPath(b.path), 'utf8')))
      return
    }
    if (url.pathname === '/write_config') {
      const p = configPath(b.path)
      writeFileSync(p + '.tmp', b.content)
      renameSync(p + '.tmp', p)
      res.writeHead(204).end()
      return
    }
    res.writeHead(404).end()
  } catch (e) {
    res.writeHead(400).end(String(e.message ?? e))
  }
}).listen(PORT, '127.0.0.1', () => console.log(`dev bridge: http://127.0.0.1:${PORT}（sidecar ${exe}）`))
