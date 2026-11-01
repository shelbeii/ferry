<script setup lang="ts">
// 编程软件（经 ferry mcp）请求部署、启停、回滚、启停 Ferry 装的软件、改防火墙、断开 SSH 连接、现在备份、新建项目时弹出确认。同意后由桌面版当作普通任务执行（任务面板里看得到、能停止），
// 进展和结果交回编程软件。执行的命令按请求里的操作、目标、应用在这里拼，不用请求方给的命令。
// 一直挂着 ferry mcp-approvals 收请求；ferry serve 重启后重新挂上。
//
// 这里还记着（只在这次打开 Ferry 期间）：
//   - 授权：「30 分钟内，这次连接对这个目标的这种操作不再询问」
//   - 拒绝过的：同一个请求 2 分钟内再来直接拒绝；选了「这次连接不再接受」的，这次连接的请求都直接拒绝
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { ElMessage, ElNotification } from 'element-plus'
import { getCurrentWindow, UserAttentionType } from '@tauri-apps/api/window'
import { call, ferry, type FerryLine, type Running } from '../ferry'
import { isTauri } from '../platform'
import { addProject, notifyUser, startTask, type Task } from '../state'
import SecretForm, { type SecretField, type SecretValue } from './SecretForm.vue'

interface McpRequest {
  id: string
  time: string
  session?: string
  client?: string
  action:
    | 'deploy' | 'start' | 'stop' | 'restart' | 'rollback' | 'sw-start' | 'sw-stop' | 'sw-restart' | 'port-open' | 'port-close' | 'fw-enable' | 'ssh-kick' | 'backup-run' | 'create'
    | 'exec' | 'write-file' | 'install' | 'tpl-save' | 'secret' | 'sw-upgrade' | 'sw-restore' | 'sw-backup'
  project: string
  config: string
  target: string
  host: string
  apps?: string[]
  skipBuild?: boolean
  to?: string
  software?: string
  /** port-*：放行 / 关闭的端口，如 8080/tcp */
  port?: string
  /** fw-enable：打开防火墙前先放行的端口；打开后重启 Docker */
  allow?: string[]
  restartDocker?: boolean
  /** ssh-kick：要断开的 SSH 连接 */
  pids?: number[]
  /** backup-run：备份哪一项（空 = 全部） */
  backup?: string
  /** create：向导的内容（原样交给 ferry create）与按它生成的 ferry.yaml（桌面版这边生成的） */
  spec?: unknown
  content?: string
  /** exec、write-file、install、tpl-save：用服务器助手的工具执行（tool + input）；detail 是 Ferry 按参数算出的确认内容 */
  tool?: string
  input?: Record<string, unknown>
  danger?: string
  /** secret：让用户填的项（值只交给 Ferry，编程软件只拿到代号） */
  fields?: SecretField[]
  /** 用户已经在编程软件里确认（设置里选了在编程软件里确认时） */
  clientConfirmed?: boolean
  detail?: string[]
  reason?: string
}

const TIMEOUT_MS = 5 * 60 * 1000
const GRANT_MS = 30 * 60 * 1000
const REJECT_REPEAT_MS = 2 * 60 * 1000
const VERBS: Record<string, string> = {
  deploy: '部署',
  start: '启动',
  stop: '停止',
  restart: '重启',
  rollback: '回滚',
  'sw-start': '启动软件',
  'sw-stop': '停止软件',
  'sw-restart': '重启软件',
  'port-open': '放行端口',
  'port-close': '关闭端口',
  'fw-enable': '打开防火墙',
  'ssh-kick': '断开 SSH 连接',
  'backup-run': '现在备份',
  create: '新建项目',
  exec: '执行命令',
  'write-file': '写文件',
  install: '安装软件',
  'tpl-save': '保存软件模板',
  secret: '填写密码',
  'sw-upgrade': '升级软件',
  'sw-restore': '恢复软件',
  'sw-backup': '设置数据备份',
}
// 用服务器助手的工具执行的操作：确认内容按命令、差异整块显示，每次都要确认
const AI_ACTIONS = ['exec', 'write-file', 'install', 'tpl-save', 'sw-upgrade', 'sw-restore', 'sw-backup']
const isAI = (r: McpRequest) => AI_ACTIONS.includes(r.action)

const queue = ref<McpRequest[]>([])
const current = computed(() => queue.value[0])
// 关闭动画期间还显示刚才那个请求（不然框里先空了）
const shown = ref<McpRequest>()
watch(current, (r) => {
  if (r) {
    shown.value = r
    grant.value = false
  }
})
const deciding = ref(false)
const grant = ref(false)
const now = ref(Date.now())
const left = computed(() => {
  if (!current.value) return ''
  const ms = new Date(current.value.time).getTime() + TIMEOUT_MS - now.value
  if (ms <= 0) return '已超时'
  const s = Math.ceil(ms / 1000)
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')} 后自动拒绝`
})

const grants = new Map<string, number>() // 授权到期时间
const rejected = new Map<string, number>() // 拒绝的时间
const blocked = new Set<string>() // 不再接受请求的连接
const watchers = new Map<number, () => void>() // 执行中的任务：步骤变了就报给编程软件
const keyOf = (r: McpRequest) =>
  [r.session, r.config, r.target, r.action, (r.apps ?? []).join(','), r.software, r.port, (r.allow ?? []).join(','), (r.pids ?? []).join(','), r.backup].join('|')
const grantKey = (r: McpRequest) => [r.session, r.config, r.target, r.action].join('|')
const clientOf = (r: McpRequest) => r.client || '编程软件'
// 改防火墙、断开别人的连接、新建项目每次都问：不给「不再询问」
const canGrant = (r: McpRequest) =>
  r.action !== 'create' && !r.action.startsWith('port-') && r.action !== 'fw-enable' && r.action !== 'ssh-kick' && r.action !== 'secret' && !isAI(r)
const titleOf = (r: McpRequest) => {
  if (r.action === 'create') return `新建项目 ${r.project}`
  if (r.action.startsWith('sw-')) return `${VERBS[r.action]} ${r.software ?? String(r.input?.name ?? '')}`
  if (r.action.startsWith('port-')) return `${VERBS[r.action]} ${r.port ?? ''}`
  if (r.action === 'fw-enable') return VERBS[r.action]
  if (r.action === 'ssh-kick') return `${VERBS[r.action]}（PID ${(r.pids ?? []).join('、')}）`
  if (r.action === 'backup-run') return `${VERBS[r.action]} ${r.backup || '全部'}`
  if (r.action === 'write-file') return `${VERBS[r.action]} ${String(r.input?.path ?? '')}`
  if (r.action === 'install') return `${VERBS[r.action]} ${String(r.input?.template || '（自定义模板）')}`
  if (r.action === 'secret') return `${VERBS[r.action]}：${(r.fields ?? []).map((f) => f.label).join('、')}`
  if (isAI(r)) return VERBS[r.action]
  return `${VERBS[r.action] ?? r.action} ${(r.apps ?? []).join('、')}`
}

let running: Running<unknown> | undefined
let stopped = false
let tick: number | undefined
let retry: number | undefined

function subscribe() {
  if (stopped) return
  running = ferry(['mcp-approvals'], (l) => {
    const line = l as unknown as Record<string, unknown>
    if (l.kind === 'mcp-request') arrive(line as unknown as McpRequest)
    else if (l.kind === 'mcp-gone') queue.value = queue.value.filter((r) => r.id !== line.id)
  })
  running.done.then(() => {
    queue.value = []
    if (!stopped) retry = window.setTimeout(subscribe, 3000)
  })
}

async function arrive(r: McpRequest) {
  const session = r.session ?? ''
  if (session && blocked.has(session)) {
    await reply(r, false, `用户已拒绝 ${clientOf(r)} 这次连接的所有改动请求`)
    return
  }
  const last = rejected.get(keyOf(r))
  if (last && Date.now() - last < REJECT_REPEAT_MS) {
    await reply(r, false, '2 分钟内刚拒绝过同样的请求')
    return
  }
  if (r.clientConfirmed) {
    // 用户已经在编程软件里确认过（Ferry 检查过设置里允许这样）：直接执行，告诉用户一声
    if (await reply(r, true, `用户已在 ${clientOf(r)} 里确认`)) {
      execute(r)
      ElNotification({ type: 'info', title: `${clientOf(r)} 请求的操作开始执行`, message: `${titleOf(r)}（${r.target}）：你在 ${clientOf(r)} 里同意了`, duration: 6000 })
    }
    return
  }
  const until = session && canGrant(r) ? grants.get(grantKey(r)) : undefined
  if (until && until > Date.now()) {
    if (await reply(r, true, '按用户的授权自动同意（30 分钟内同类不再询问）')) {
      execute(r)
      ElNotification({ type: 'info', title: `${clientOf(r)} 请求的操作已自动执行`, message: `${titleOf(r)}（${r.target}）：按你 30 分钟内不再询问的授权`, duration: 6000 })
    }
    return
  }
  queue.value.push(r)
  attention()
  notifyUser(`${clientOf(r)} 请求改动服务器`, `${titleOf(r)}（${r.target}）：到 Ferry 里同意或拒绝，5 分钟内有效`, true)
}

async function attention() {
  if (!isTauri) return
  try {
    const w = getCurrentWindow()
    await w.unminimize()
    await w.show()
    await w.setFocus()
    await w.requestUserAttention(UserAttentionType.Critical)
  } catch {
    // 拿不到焦点也没关系：确认框已经显示
  }
}

/** 把决定交回去；请求已经不在了（超时、编程软件取消了）时返回 false。 */
async function reply(r: McpRequest, approve: boolean, note = ''): Promise<boolean> {
  try {
    await call(['mcp-decide', r.id, approve ? 'approve' : 'reject', ...(note ? ['--note', note] : [])])
    return true
  } catch (e) {
    ElMessage.warning((e as Error).message)
    return false
  }
}

async function decide(approve: boolean, blockSession = false) {
  const r = current.value
  if (!r) return
  deciding.value = true
  try {
    if (approve) {
      if (await reply(r, true, grant.value ? '用户同意，并授权 30 分钟内同类不再询问' : '')) {
        if (grant.value && r.session && canGrant(r)) grants.set(grantKey(r), Date.now() + GRANT_MS)
        execute(r)
      }
    } else {
      rejected.set(keyOf(r), Date.now())
      if (blockSession && r.session) blocked.add(r.session)
      await reply(r, false, blockSession ? `你在 Ferry 里拒绝了，并且不再接受 ${clientOf(r)} 这次连接的改动请求` : '')
    }
  } finally {
    queue.value = queue.value.filter((x) => x.id !== r.id)
    deciding.value = false
  }
}

const scopeOf = (r: McpRequest) => `mcp-${r.session ?? ''}`

/** 填写密码：值交给 Ferry（只记在这次打开的 Ferry 里），编程软件只拿到代号。 */
async function fill(values: SecretValue[]) {
  const r = current.value
  if (!r) return
  deciding.value = true
  try {
    await call(['ai-secret', '--scope', scopeOf(r)], { FERRY_AI_SECRETS: JSON.stringify(values) })
    if (await reply(r, true)) {
      const output =
        '用户在 Ferry 里填好了（值只在用户电脑上，你看不到，也不要问）：\n' +
        (r.fields ?? [])
          .map((f) => `- ${f.label}：代号 {{secret:${f.name}}}，run_change_command 的命令里用 "$FERRY_SECRET_${f.name.toUpperCase()}"，write_file 的内容、software_install 的参数里写代号`)
          .join('\n')
      call(['mcp-decide', r.id, 'done'], { FERRY_MCP_OUTCOME: JSON.stringify({ ok: true, data: { output } }) }).catch(() => {})
    }
  } catch (e) {
    ElMessage.error((e as Error).message)
    return
  } finally {
    deciding.value = false
  }
  queue.value = queue.value.filter((x) => x.id !== r.id)
}

/** 按请求里的操作、目标、应用拼命令，当作普通任务执行；进展和结果交回编程软件。 */
function execute(r: McpRequest) {
  if (r.action === 'create') {
    createProject(r)
    return
  }
  if (isAI(r)) {
    const title = `${titleOf(r)} ${r.target}（${clientOf(r)} 请求）`
    const args = ['ai-exec', r.target, r.tool ?? '', '-c', r.config, '--scope', scopeOf(r)]
    const task: Task = startTask(title, args, (t) => finish(r, t), { FERRY_VIA: `${clientOf(r)} · MCP`, FERRY_AI_INPUT: JSON.stringify(r.input ?? {}) })
    watchProgress(r, task)
    return
  }
  const apps = r.apps ?? []
  let args: string[]
  switch (r.action) {
    case 'sw-start':
    case 'sw-stop':
    case 'sw-restart':
      args = ['software', r.action.slice(3), r.target, r.software ?? '', '-c', r.config]
      break
    case 'port-open':
    case 'port-close':
      args = ['port', r.action.slice(5), r.target, r.port ?? '', '-c', r.config, '--yes']
      break
    case 'fw-enable':
      args = ['port', 'enable', r.target, ...(r.allow?.length ? ['--allow', r.allow.join(',')] : []), ...(r.restartDocker ? ['--restart-docker'] : []), '-c', r.config, '--yes']
      break
    case 'ssh-kick':
      args = ['who', 'kick', r.target, ...(r.pids ?? []).map(String), '-c', r.config, '--yes']
      break
    case 'backup-run':
      args = ['backup', r.target, 'run', ...(r.backup ? [r.backup] : []), '-c', r.config]
      break
    case 'deploy':
      args = ['deploy', r.target, apps.join(','), '-c', r.config, '--yes', ...(r.skipBuild ? ['--skip-build'] : [])]
      break
    case 'rollback':
      args = ['rollback', r.target, apps[0], '-c', r.config, '--yes', ...(r.to ? ['--to', r.to] : [])]
      break
    default:
      args = [r.action, r.target, apps.join(','), '-c', r.config, '--yes']
  }
  const what = r.action.startsWith('sw-')
    ? (r.software ?? '')
    : r.action.startsWith('port-')
      ? (r.port ?? '')
      : r.action === 'fw-enable'
        ? `放行 ${r.allow?.length ? r.allow.join('、') : 'SSH 端口'}`
        : r.action === 'ssh-kick'
          ? `PID ${(r.pids ?? []).join('、')}`
          : r.action === 'backup-run'
            ? r.backup || '全部'
            : apps.join('、')
  const title = `${VERBS[r.action]} ${r.target}：${what}（${clientOf(r)} 请求）`
  const task: Task = startTask(title, args, (t) => finish(r, t), { FERRY_VIA: `${clientOf(r)} · MCP` })
  watchProgress(r, task)
}

/** 当前步骤变了就报一行给编程软件（最多每秒一行）。 */
function watchProgress(r: McpRequest, task: Task) {
  let lastSent = ''
  let lastAt = 0
  const stopWatch = watch(
    () => task.step?.text,
    (text) => {
      if (!text || text === lastSent || Date.now() - lastAt < 1000) return
      lastSent = text
      lastAt = Date.now()
      call(['mcp-decide', r.id, 'progress', '--note', text]).catch(() => {})
    },
  )
  watchers.set(task.id, stopWatch)
}

/** 新建项目：按向导的内容生成 ferry.yaml（不覆盖已有文件），加进项目列表。 */
async function createProject(r: McpRequest) {
  const outcome: { ok: boolean; error?: string; data?: unknown } = { ok: false }
  try {
    const res = await call<{ path: string }>(['create', '-c', r.config], { FERRY_CREATE_VALUE: JSON.stringify(r.spec) })
    await addProject({ path: r.config, name: r.project })
    outcome.ok = true
    outcome.data = { path: res.path, project: r.project, note: '已加进 Ferry 的项目列表；新服务器第一次连接前要在 Ferry 里核对主机公钥' }
    ElMessage.success(`已新建项目 ${r.project}（${clientOf(r)} 请求）`)
  } catch (e) {
    outcome.error = (e as Error).message
    ElMessage.error(`新建项目 ${r.project} 失败：${outcome.error}`)
  }
  call(['mcp-decide', r.id, 'done'], { FERRY_MCP_OUTCOME: JSON.stringify(outcome) }).catch(() => {})
}

function lineText(l: FerryLine): string {
  const head = [l.kind === 'event' ? l.code : l.kind === 'step' ? '==>' : l.kind === 'error' ? '✗' : l.kind === 'warn' ? '!' : '', l.app]
    .filter(Boolean)
    .join(' ')
  return `${head ? head + ' ' : ''}${l.msg ?? ''}`.trim()
}

function finish(r: McpRequest, t: Task) {
  watchers.get(t.id)?.()
  watchers.delete(t.id)
  const outcome = {
    ok: t.status === 'ok',
    error: t.status === 'ok' ? undefined : t.error || '失败',
    data: t.data,
    log: t.lines.slice(-60).map(lineText).filter(Boolean),
  }
  call(['mcp-decide', r.id, 'done'], { FERRY_MCP_OUTCOME: JSON.stringify(outcome) }).catch(() => {})
}

onMounted(() => {
  subscribe()
  tick = window.setInterval(() => (now.value = Date.now()), 1000)
})
onBeforeUnmount(() => {
  stopped = true
  window.clearInterval(tick)
  window.clearTimeout(retry)
  running?.kill()
})

// 确认内容第一行之后的部分（命令、差异）按行显示
const detailLines = (d: string[]) => d.slice(1).join('\n').split('\n')

const actionType = (a: string) =>
  ['stop', 'rollback', 'sw-stop', 'port-close', 'port-open', 'fw-enable', 'ssh-kick'].includes(a)
    ? 'danger'
    : a === 'create' || a === 'backup-run' || a === 'secret'
      ? 'primary'
      : 'warning'
</script>

<template>
  <el-dialog
    :model-value="!!current"
    :title="shown?.action === 'create' ? '编程软件请求新建项目' : shown?.action === 'secret' ? '编程软件请你填写' : '编程软件请求改动服务器'"
    width="560px"
    :close-on-click-modal="false"
    :close-on-press-escape="false"
    :show-close="false"
    align-center
    append-to-body
  >
    <template v-if="shown">
      <div class="headline">
        <el-tag :type="actionType(shown.action)" disable-transitions>{{ titleOf(shown) }}</el-tag>
        <el-tag v-if="shown.danger" type="danger" effect="dark" disable-transitions>危险：{{ shown.danger }}</el-tag>
        <span class="muted small">来自 {{ clientOf(shown) }}</span>
      </div>
      <el-descriptions :column="1" border size="small" label-width="80px">
        <el-descriptions-item label="项目">{{ shown.project }}</el-descriptions-item>
        <el-descriptions-item v-if="shown.action === 'create'" label="配置文件">
          <span class="mono">{{ shown.config }}</span>
        </el-descriptions-item>
        <el-descriptions-item v-else label="服务器">
          {{ shown.target }}<span class="muted mono ml">{{ shown.host }}</span>
        </el-descriptions-item>
        <el-descriptions-item v-for="(d, i) in isAI(shown) || shown.action === 'secret' ? [] : (shown.detail ?? [])" :key="i" :label="i === 0 ? '详情' : ''">
          <span class="detail">{{ d }}</span>
        </el-descriptions-item>
        <el-descriptions-item label="理由">
          <span v-if="shown.reason" class="reason">{{ shown.reason }}</span>
          <span v-else class="muted">（没有说明）</span>
        </el-descriptions-item>
      </el-descriptions>
      <div v-if="isAI(shown) && shown.detail?.length" class="yaml">
        <div class="muted small">{{ shown.detail[0] }}</div>
        <pre class="mono"><span v-for="(ln, k) in detailLines(shown.detail)" :key="k" :class="{ add: ln.startsWith('+ '), del: ln.startsWith('- ') }">{{ ln }}
</span></pre>
      </div>
      <SecretForm v-if="shown.action === 'secret' && shown.fields?.length" class="secret" :fields="shown.fields" :disabled="deciding" no-remember @submit="fill" @cancel="decide(false)" />
      <div v-if="shown.action === 'create' && shown.content" class="yaml">
        <div class="muted small">将写入的 ferry.yaml（已有文件不会覆盖）：</div>
        <pre class="mono">{{ shown.content }}</pre>
      </div>
      <el-checkbox v-if="canGrant(shown)" v-model="grant" class="grant">
        30 分钟内，{{ clientOf(shown) }} 这次连接对 {{ shown.target }} 的「{{ VERBS[shown.action] }}」不再询问
      </el-checkbox>
      <p class="muted small note">
        <template v-if="shown.action === 'create'">同意后生成这个文件，并加进 Ferry 的项目列表；不连服务器、不部署。</template>
        <template v-else-if="shown.action === 'secret'">填的内容只记在这次打开的 Ferry 里，执行时才用上；{{ clientOf(shown) }} 只拿到代号，看不到内容。Ferry 退出或这次连接断开后要重新填。</template>
        <template v-else>同意后由 Ferry 执行，任务面板里能看到过程、可以停止；结果交回编程软件，部署记录里会标明经它请求。</template>
        看不明白要做什么时请拒绝。
        <template v-if="queue.length > 1">后面还有 {{ queue.length - 1 }} 个请求。</template>
      </p>
    </template>
    <template #footer>
      <span class="muted small left">{{ left }}</span>
      <el-dropdown split-button :disabled="deciding" trigger="click" @click="decide(false)" @command="decide(false, true)">
        拒绝
        <template #dropdown>
          <el-dropdown-menu>
            <el-dropdown-item command="block">拒绝，并且这次连接不再接受改动请求</el-dropdown-item>
          </el-dropdown-menu>
        </template>
      </el-dropdown>
      <el-button v-if="shown?.action !== 'secret'" type="primary" class="ml" :loading="deciding" @click="decide(true)">同意执行</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.headline {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-bottom: 12px;
}
.small {
  font-size: 12px;
}
.ml {
  margin-left: 8px;
}
.reason,
.detail {
  white-space: pre-wrap;
  word-break: break-all;
}
.grant {
  margin-top: 10px;
}
.yaml {
  margin-top: 10px;
}
.yaml pre {
  margin: 4px 0 0;
  padding: 8px 10px;
  max-height: 220px;
  overflow: auto;
  font-size: 12px;
  border: 1px solid var(--el-border-color);
  border-radius: 6px;
  background: var(--el-fill-color-light);
}
.note {
  margin: 6px 0 0;
}
.yaml pre .add {
  color: var(--el-color-success);
}
.yaml pre .del {
  color: var(--el-color-danger);
}
.secret {
  margin-top: 12px;
}
.left {
  float: left;
  line-height: 32px;
}
</style>
