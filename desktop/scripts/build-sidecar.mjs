// 构建 Tauri sidecar：先交叉编译 ferry-agent（linux amd64/arm64，内嵌进 ferry），再编译本机的 ferry，
// 输出到 src-tauri/binaries/ferry-<Rust 目标三元组>[.exe]（Tauri externalBin 的命名要求）。
import { execFileSync } from 'node:child_process'
import { mkdirSync, readFileSync } from 'node:fs'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const here = dirname(fileURLToPath(import.meta.url))
const repo = resolve(here, '..', '..')
const outDir = resolve(here, '..', 'src-tauri', 'binaries')

const run = (cmd, args, env = {}) =>
  execFileSync(cmd, args, { cwd: repo, stdio: 'inherit', env: { ...process.env, ...env } })

const rustInfo = execFileSync('rustc', ['-vV'], { encoding: 'utf8' })
const host = /host: (\S+)/.exec(rustInfo)?.[1]
if (!host) throw new Error('无法从 rustc -vV 读取目标三元组')
// tauri build --target <三元组> 时 Tauri 通过环境变量告诉我们目标（如在 Apple 芯片的机器上打 Intel 版）
const triple = process.env.TAURI_ENV_TARGET_TRIPLE || host
const exe = triple.includes('windows') ? '.exe' : ''

// 三元组 → Go 的 GOOS / GOARCH（与本机相同时留空，用 Go 自己的默认值）
function goTarget(t) {
  if (t === host) return { GOOS: '', GOARCH: '' }
  const arch = t.startsWith('aarch64') ? 'arm64' : t.startsWith('x86_64') ? 'amd64' : ''
  const os = t.includes('apple-darwin') ? 'darwin' : t.includes('windows') ? 'windows' : t.includes('linux') ? 'linux' : ''
  if (!arch || !os) throw new Error(`不支持的目标：${t}`)
  return { GOOS: os, GOARCH: arch }
}

// 版本：桌面版的版本号 + 构建时的提交（语义化版本的构建元数据），如 0.4.5+17f7314 / 0.4.5+17f7314.dirty
const pkg = JSON.parse(readFileSync(resolve(here, '..', 'package.json'), 'utf8'))
let version = pkg.version
try {
  const commit = execFileSync('git', ['describe', '--always', '--dirty=.dirty', '--exclude', '*'], { cwd: repo, encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] }).trim()
  if (commit) version += '+' + commit
} catch {}

for (const arch of ['amd64', 'arm64']) {
  // agent 不写入应用版本号：代码没变时内容完全相同，升级 Ferry 后不用重新上传到服务器
  run('go', ['build', '-trimpath', '-buildvcs=false', '-ldflags', '-s -w',
    '-o', join('internal', 'agentbin', 'bin', `ferry-agent-linux-${arch}`), './cmd/ferry-agent'],
    { CGO_ENABLED: '0', GOOS: 'linux', GOARCH: arch })
}
mkdirSync(outDir, { recursive: true })
const out = join(outDir, `ferry-${triple}${exe}`)
run('go', ['build', '-trimpath', '-ldflags', `-s -w -X main.Version=${version}`, '-o', out, './cmd/ferry'],
  { CGO_ENABLED: '0', ...goTarget(triple) })
console.log(`sidecar: ${out}`)
