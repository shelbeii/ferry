<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { Loading, RefreshRight, Search } from '@element-plus/icons-vue'
import { tasks, drawerOpen, selectedTaskId as selectedId, loadTaskDetail, removeTasks, askFixParents, askPkgChanges, aiAskRequest, currentPath, selectProject, type Task } from '../state'
import { bytes, call, evLabel, padWidth, type FerryLine, type DeployReport, type AppOutcome } from '../ferry'
import { ElMessage } from 'element-plus'

const query = ref('')
const statusFilter = ref('all')
const filteredTasks = computed(() => tasks.filter((t) => (statusFilter.value === 'all' || (statusFilter.value === 'finished' ? t.status !== 'running' : t.status === statusFilter.value)) && `${t.title} ${t.target ?? ''} ${t.error ?? ''}`.toLowerCase().includes(query.value.trim().toLowerCase())))
const selected = computed<Task | undefined>(() => filteredTasks.value.find((t) => t.id === selectedId.value) ?? filteredTasks.value[0])
watch(
  () => tasks.map((t) => t.id),
  () => { if (!tasks.some((t) => t.id === selectedId.value)) selectedId.value = tasks[0]?.id ?? null },
)
watch(selectedId, (id) => { if (tasks.some((t) => t.id === id) && !filteredTasks.value.some((t) => t.id === id)) { query.value = ''; statusFilter.value = 'all' } })
// 以前的任务：选中时才读输出
watch(
  () => (drawerOpen.value ? selected.value : undefined),
  (t) => t && loadTaskDetail(t),
  { immediate: true },
)

const logBox = ref<HTMLElement>()
const followOutput = ref(true)
const recentStages = computed(() => (selected.value?.lines ?? []).filter((l) => l.kind === 'step' && l.msg).slice(-5))
watch(() => selected.value?.id, () => { followOutput.value = true; followLatest() })
function onOutputScroll() {
  const box = logBox.value
  if (box) followOutput.value = box.scrollHeight - box.scrollTop - box.clientHeight < 40
}
async function followLatest() {
  followOutput.value = true
  await nextTick()
  if (logBox.value) logBox.value.scrollTop = logBox.value.scrollHeight
}
watch(
  () => selected.value?.received ?? selected.value?.lines.length, // 行数到上限后不再变：看收到的总行数
  async () => {
    await nextTick()
    if (logBox.value && followOutput.value) logBox.value.scrollTop = logBox.value.scrollHeight
  },
)

const switching = ref(false)
const cancelling = ref(new Set<number>())
async function cancelTask(t: Task) {
  if (!t.kill || cancelling.value.has(t.id)) return
  cancelling.value.add(t.id)
  try { await t.kill() } catch (e) { ElMessage.error((e as Error).message) } finally { cancelling.value.delete(t.id) }
}

/** java 版本不够：改写配置让应用用 Ferry 提供的 JDK，然后重新执行这个部署 */
async function useRuntime(t: Task) {
  const d = t.detail
  if (!d?.app || !d.need || !t.config) return
  switching.value = true
  try {
    await call(['runtime', 'use', d.app, '--java', String(d.need), '-c', t.config])
    ElMessage.success(`已把 ${d.app} 改为使用 Ferry 提供的 JDK ${d.need}，重新部署`)
    t.retry?.()
  } catch (e) {
    ElMessage.error((e as Error).message)
  } finally {
    switching.value = false
  }
}

/** 运行用户进不了上级目录：确认后加上权限重新安装 */
async function fixParents(t: Task) {
  if (t.detail && (await askFixParents(t.detail))) t.retryWith?.(['--fix-parents'])
}

/** 装包会顺带改动已装的包：确认后接着安装 */
async function allowChanges(t: Task) {
  if (t.detail && (await askPkgChanges(t.detail))) t.retryWith?.(['--allow-changes'])
}

function lineClass(l: FerryLine): string {
  if (l.kind === 'build') return 'ev-build'
  if (l.kind === 'step') return 'ev-step'
  if (l.kind === 'error' || l.level === 'error') return 'ev-error'
  if (l.kind === 'warn' || l.level === 'warn') return 'ev-warn'
  if (l.code === 'READY') return 'ev-ok'
  if (l.code === 'ROLLBACK') return 'ev-rollback'
  return ''
}

/** 预检等带修复建议的消息：「说明\n修复：命令」拆成两部分 */
function splitFix(l: FerryLine): [string, string] | undefined {
  if ((l.kind !== 'error' && l.kind !== 'warn') || !l.msg) return undefined
  const i = l.msg.indexOf('\n修复：')
  return i < 0 ? undefined : [l.msg.slice(0, i), l.msg.slice(i + 4)]
}

// 失败的任务交给 AI：带上出错的原因和输出的最后几十行，在这个项目、这台服务器的 AI 助手里新开一个对话
async function askAI(t: Task) {
  if (currentPath.value !== t.config) await selectProject(t.config!) // 换到这个项目，它的页面接着打开 AI 助手
  const tail = t.lines.slice(-40).map(lineText).join('\n')
  aiAskRequest.value = {
    config: t.config!,
    target: t.target!,
    text: `「${t.title}」失败了${t.error ? '：' + t.error : ''}。
输出的最后几行：
${tail}

请查看服务器上的情况（应用状态、日志、端口、磁盘），找出失败的原因，告诉我怎么修。先只查看，不要改动。`,
  }
  drawerOpen.value = false
}

function lineText(l: FerryLine): string {
  if (l.kind === 'event') return `${padWidth(evLabel(l.code), 8)} ${padWidth(l.app || '-', 12)} ${l.msg ?? ''}`
  if (l.kind === 'step') return `==> ${l.msg}`
  return l.msg ?? ''
}

/** 今天的只显示时间；以前的（任务记录里的）带上日期 */
function clock(ts: number): string {
  const d = new Date(ts)
  if (d.toDateString() === new Date().toDateString()) return d.toLocaleTimeString()
  return `${d.getMonth() + 1}-${d.getDate()} ${d.toLocaleTimeString()}`
}

// 每秒刷新一次（进行中的计时、无输出提示）
const now = ref(Date.now())
let timer: number | undefined
onMounted(() => (timer = window.setInterval(() => (now.value = Date.now()), 1000)))
onBeforeUnmount(() => clearInterval(timer))

function dur(ms: number): string {
  const s = Math.max(0, Math.round(ms / 1000))
  return s >= 60 ? `${Math.floor(s / 60)}分${s % 60}秒` : `${s}秒`
}

function elapsed(t: Task): string {
  return dur((t.endedAt ?? now.value) - t.startedAt)
}

/** 多久没有新输出（秒） */
const idle = computed(() => (selected.value && selected.value.status === 'running' ? Math.floor((now.value - selected.value.lastAt) / 1000) : 0))

function speedText(p: NonNullable<Task['progress']>): string {
  if (!p.speed) return '计算速度…'
  const eta = p.eta !== undefined ? (p.eta >= 60 ? `剩余约 ${Math.ceil(p.eta / 60)} 分钟` : `剩余约 ${Math.max(1, Math.round(p.eta))} 秒`) : ''
  return `${bytes(p.speed)}/s${eta ? ' · ' + eta : ''}`
}

// 部署结果：每个目标一张表
const reports = computed<DeployReport[]>(() => {
  const d = selected.value?.data
  return Array.isArray(d) && d.length > 0 && typeof d[0] === 'object' && d[0] !== null && 'release' in d[0] ? (d as DeployReport[]) : []
})
const outcomes = computed<AppOutcome[]>(() => {
  const d = selected.value?.data
  return Array.isArray(d) && d.length > 0 && typeof d[0] === 'object' && d[0] !== null && 'result' in d[0] && !('release' in d[0] && 'git' in d[0])
    ? (d as AppOutcome[])
    : []
})

function resultType(r: string): 'success' | 'danger' | 'warning' | 'info' {
  if (r === 'OK') return 'success'
  if (r === 'FAILED' || r === 'ROLLBACK_FAILED') return 'danger'
  if (r === 'ROLLED_BACK' || r === 'SKIPPED') return 'warning'
  return 'info'
}

// 说明：agent 没给说明时（成功的启停等），按任务和结果补一句
const resultText: Record<string, string> = {
  FAILED: '失败',
  ROLLED_BACK: '新版本没有就绪，已切回上一版本',
  ROLLBACK_FAILED: '失败，切回上一版本也失败了：请查看上面的输出',
  SKIPPED: '跳过',
}
function explain(o: AppOutcome): string {
  if (o.msg) return o.msg
  if (o.result !== 'OK') return resultText[o.result] ?? ''
  const t = selected.value?.title ?? ''
  const rel = o.release ? `：${o.release}` : ''
  if (t.startsWith('停止')) return '已停止'
  if (t.startsWith('启动')) return '已启动，运行正常'
  if (t.startsWith('重启')) return '已重启，运行正常'
  if (t.startsWith('回滚')) return `已回滚${rel}`
  if (t.startsWith('部署')) {
    let ready = ''
    if (o.readySec) ready = o.prevReadySec ? `，启动用了 ${o.readySec} 秒（上次 ${o.prevReadySec} 秒）` : `，启动用了 ${o.readySec} 秒`
    if (o.errorLines) ready += o.prevErrorLines !== undefined && o.prevErrorLines >= 0 ? `，启动时 ${o.errorLines} 行错误（上次 ${o.prevErrorLines} 行）` : `，启动时 ${o.errorLines} 行错误`
    return `已部署${rel}${ready}`
  }
  return '完成'
}

/** 已完成（含失败、中断）的从面板和任务记录里一起删掉 */
async function clearFinished() {
  try {
    await removeTasks(tasks.filter((t) => t.status !== 'running'))
  } catch (e) {
    ElMessage.error(`删除任务记录失败：${(e as Error).message}`)
  }
}
</script>

<template>
  <el-drawer v-model="drawerOpen" title="任务中心" size="min(1080px, 94vw)" direction="rtl" append-to-body>
    <div class="wrap">
      <div class="list">
        <div class="list-head">
          <span class="muted">{{ filteredTasks.length }} / {{ tasks.length }} 个</span>
          <el-button text size="small" :disabled="!tasks.some((t) => t.status !== 'running')" @click="clearFinished">清除已完成</el-button>
        </div>
        <el-input v-model="query" :prefix-icon="Search" placeholder="搜索任务或服务器" clearable size="small" aria-label="搜索任务" />
        <el-select v-model="statusFilter" size="small" class="task-filter" aria-label="任务状态"><el-option label="全部任务" value="all" /><el-option label="进行中" value="running" /><el-option label="失败" value="failed" /><el-option label="已结束" value="finished" /></el-select>
        <div
          v-for="t in filteredTasks"
          :key="t.id"
          class="item"
          :class="{ active: selected && t.id === selected.id }"
          @click="selectedId = t.id"
          role="button"
          :aria-pressed="selected?.id === t.id"
          tabindex="0"
          @keydown.enter="selectedId = t.id"
          @keydown.space.prevent="selectedId = t.id"
        >
          <div class="item-title">{{ t.title }}</div>
          <div class="item-meta">
            <el-tag v-if="t.status === 'running'" size="small" class="running-tag"><el-icon class="spin"><Loading /></el-icon>进行中</el-tag>
            <el-tag v-else-if="t.status === 'ok'" size="small" type="success">完成</el-tag>
            <el-tag v-else-if="t.status === 'interrupted'" size="small" type="warning">中断</el-tag>
            <el-tag v-else size="small" type="danger">失败</el-tag>
            <span class="muted">{{ clock(t.startedAt) }} · {{ elapsed(t) }}</span>
          </div>
        </div>
        <el-empty v-if="!filteredTasks.length" :description="tasks.length ? '没有匹配的任务' : '还没有任务'" :image-size="50" />
      </div>
      <div v-if="selected" class="detail">
        <div class="detail-head">
          <strong>{{ selected.title }}</strong>
          <el-button v-if="selected.kill" size="small" type="danger" plain :loading="cancelling.has(selected.id)" @click="cancelTask(selected)">{{ cancelling.has(selected.id) ? '正在停止…' : '取消任务' }}</el-button>
        </div>
        <div class="detail-meta"><el-tag :type="selected.status === 'ok' ? 'success' : selected.status === 'failed' ? 'danger' : selected.status === 'interrupted' ? 'warning' : 'primary'" size="small">{{ { running: '进行中', ok: '完成', failed: '失败', interrupted: '中断' }[selected.status] }}</el-tag><span>{{ clock(selected.startedAt) }} · 用时 {{ elapsed(selected) }}</span><span v-if="selected.target" class="mono">{{ selected.target }}</span></div>
        <el-alert v-if="selected.detailLoadError" :title="selected.detailLoadError" type="error" :closable="false"><el-button link type="primary" @click="loadTaskDetail(selected)">重新读取</el-button></el-alert>
        <el-alert
          v-if="selected.kill"
          type="info"
          :closable="false"
          show-icon
          title="取消只会停止本机这一侧；服务器上已经开始的切换会继续完成（不会停在一半）。"
          class="tip"
        />
        <!-- 进行中：当前在做什么、这一步已用时间；上传时显示速度与剩余时间 -->
        <div v-if="selected.status === 'running'" class="live">
          <div class="live-head">
            <el-icon class="spin live-icon"><Loading /></el-icon>
            <!-- 步骤文字占满剩余宽度、过长省略（悬停看全文）；右侧时间固定不折行、不被挤压 -->
            <el-tooltip :show-after="HOVER_DELAY" :content="selected.step?.text || '进行中'" placement="bottom">
              <span class="live-step">{{ selected.step?.text || '进行中' }}</span>
            </el-tooltip>
            <span class="live-time">
              <span>{{ selected.step ? dur(now - selected.step.at) : '' }}</span>
              <span v-if="selected.step?.waitMax">／最多 {{ selected.step.waitMax }} 秒</span>
              <span class="sep">·</span>
              <span>共 {{ elapsed(selected) }}</span>
            </span>
          </div>
          <template v-if="selected.progress">
            <el-progress
              :percentage="Math.floor((selected.progress.done * 100) / selected.progress.total)"
              :stroke-width="16"
              text-inside
              striped
              striped-flow
              :duration="12"
            />
            <div class="muted mono live-sub">
              {{ bytes(selected.progress.done) }} / {{ bytes(selected.progress.total) }} · {{ speedText(selected.progress) }}
            </div>
          </template>
          <div v-else class="pulse"><span /></div>
          <div v-if="idle >= 20" class="idle">
            已 {{ idle }} 秒没有新输出——仍在进行（例如等待服务启动、服务器处理大文件）。如果长时间没有变化，可以取消后查看日志。
          </div>
        </div>
        <div v-if="recentStages.length" class="task-stages" aria-label="最近执行阶段"><div v-for="(stage, i) in recentStages" :key="i" :class="{ current: i === recentStages.length - 1 && selected.status === 'running' }"><span>{{ String(i + 1).padStart(2, '0') }}</span><b>{{ stage.msg }}</b></div></div>
        <div class="output-heading"><span>执行输出</span><el-button size="small" text :type="followOutput ? 'primary' : 'warning'" @click="followLatest">{{ followOutput ? '跟随最新输出' : '已暂停滚动 · 回到最新' }}</el-button></div>
        <div ref="logBox" class="log mono" role="region" aria-label="任务执行输出" tabindex="0" @scroll="onOutputScroll">
          <div v-if="selected.loadingDetail" class="muted" role="status">正在读取任务输出…</div>
          <div v-else-if="!selected.lines.length" class="muted">{{ selected.status === 'running' ? '等待任务输出…' : '没有保留执行输出' }}</div>
          <div v-for="(l, i) in selected.lines" :key="i" :class="[lineClass(l), { 'has-fix': splitFix(l) }]">
            <template v-if="splitFix(l)">
              <span class="tag">{{ l.kind === 'warn' ? '提示' : '问题' }}</span>{{ splitFix(l)![0] }}
              <span class="fix"><span class="tag">修复</span>{{ splitFix(l)![1] }}</span>
            </template>
            <template v-else>{{ lineText(l) }}</template>
          </div>
        </div>
        <el-alert
          v-if="selected.error"
          :type="selected.status === 'interrupted' ? 'warning' : 'error'"
          :title="selected.error"
          :closable="false"
          show-icon
          class="tip"
        />
        <div v-if="selected.status === 'failed' && (selected.retry || (selected.config && selected.target))" class="tip">
          <!-- 在服务器上手动改好后（删掉目录、改权限……），点一下用同样的设置再执行一次 -->
          <el-button v-if="selected.retry" size="small" type="primary" :icon="RefreshRight" @click="selected.retry()">重试</el-button>
          <el-button v-if="selected.config && selected.target" size="small" type="warning" plain @click="askAI(selected)">让 AI 看看为什么失败</el-button>
        </div>
        <el-alert v-if="selected.detail?.kind === 'blocked-dirs' && selected.retryWith" type="info" :closable="false" show-icon class="tip">
          <template #title>
            可以给运行用户 {{ selected.detail.user }} 加上进入 {{ selected.detail.dirs?.join('、') }} 的权限（只能进入，看不到里面有哪些文件），然后接着安装
          </template>
          <el-button type="primary" size="small" class="fix-btn" @click="fixParents(selected)">加上权限并继续安装</el-button>
        </el-alert>
        <el-alert v-if="selected.detail?.kind === 'pkg-changes' && selected.retryWith" type="warning" :closable="false" show-icon class="tip">
          <template #title>装 {{ selected.detail.packages?.join(' ') }} 会顺带改动已装的包：{{ selected.detail.changes?.join('、') }}</template>
          <el-button type="primary" size="small" class="fix-btn" @click="allowChanges(selected)">同意并继续安装</el-button>
        </el-alert>
        <el-alert v-if="selected.detail?.kind === 'java-mismatch' && selected.config && selected.retry" type="info" :closable="false" show-icon class="tip">
          <template #title>
            可以改用 Ferry 提供的 JDK {{ selected.detail.need }}：部署时自动下载（本机已有就不下载）并上传到服务器，
            配置里 {{ selected.detail.app }} 的 java 换成 runtime: {java: {{ selected.detail.need }}}
          </template>
          <el-button type="primary" size="small" :loading="switching" class="fix-btn" @click="useRuntime(selected)">
            改用 JDK {{ selected.detail.need }} 并重新部署
          </el-button>
        </el-alert>
        <div v-for="r in reports" :key="r.target" class="result">
          <div class="muted">
            {{ r.target }} · 版本 {{ r.release }} · 上传 {{ bytes(r.upload) }}（全量 {{ bytes(r.total) }}）
          </div>
          <el-table :data="r.apps ?? []" size="small" border>
            <el-table-column prop="app" label="应用" width="140" />
            <el-table-column label="结果" width="140">
              <template #default="{ row }"><el-tag :type="resultType(row.result)" size="small">{{ row.result }}</el-tag></template>
            </el-table-column>
            <el-table-column label="说明">
              <template #default="{ row }">{{ explain(row) }}</template>
            </el-table-column>
          </el-table>
        </div>
        <el-table v-if="outcomes.length" :data="outcomes" size="small" border class="result">
          <el-table-column prop="app" label="应用" width="140" />
          <el-table-column label="结果" width="140">
            <template #default="{ row }"><el-tag :type="resultType(row.result)" size="small">{{ row.result }}</el-tag></template>
          </el-table-column>
          <el-table-column label="说明">
            <template #default="{ row }">{{ explain(row) }}</template>
          </el-table-column>
        </el-table>
      </div>
    </div>
  </el-drawer>
</template>

<style scoped>
.task-stages { display: flex; overflow-x: auto; border: 1px solid var(--ferry-stroke); border-radius: 9px; background: var(--el-fill-color-light); padding: 12px; gap: 12px; }
.task-stages > div { display: flex; flex: 1; flex-direction: column; min-width: 90px; gap: 7px; color: var(--el-text-color-secondary); }
.task-stages span { font-size: 10px; letter-spacing: 1px; }
.task-stages b { font-size: 11px; font-weight: 500; line-height: 1.5; }
.task-stages .current { color: var(--el-color-primary); }
.output-heading { display: flex; align-items: center; justify-content: space-between; gap: 12px; font-size: 11px; color: var(--el-text-color-secondary); }
.detail-meta { display: flex; flex-wrap: wrap; align-items: center; gap: 8px 12px; color: var(--el-text-color-secondary); font-size: 12px; }
.task-filter { width: 100%; margin: 8px 0; }
.wrap {
  display: flex;
  gap: 12px;
  height: 100%;
  min-height: 0;
  container: tasks / inline-size;
}
.list {
  width: 210px;
  flex-shrink: 0;
  overflow: auto;
  border-right: 1px solid var(--el-border-color-lighter);
  padding-right: 8px;
}
.list-head {
  display: flex;
  justify-content: space-between;
  align-items: center;
  margin-bottom: 6px;
}
.item {
  padding: 8px;
  border-radius: 6px;
  cursor: pointer;
  margin-bottom: 4px;
}
.item:hover {
  background: var(--el-fill-color-light);
}
.item.active {
  background: var(--el-color-primary-light-9);
}
.item-title {
  font-size: 13px;
  margin-bottom: 4px;
  word-break: break-all;
}
.item-meta {
  display: flex;
  gap: 6px;
  align-items: center;
  font-size: 12px;
}
.detail {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 8px;
  overflow-y: auto;
  padding: 2px;
}
.detail-head {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 12px;
}
.detail-head strong { overflow-wrap: anywhere; }
.detail-head .el-button { flex-shrink: 0; }
@container tasks (max-width: 760px) {
  .list { width: 170px; }
  .item-meta { flex-wrap: wrap; }
  .live-head { flex-wrap: wrap; }
  .live-time { margin-left: 24px; }
}
.live {
  flex-shrink: 0;
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 10px 12px;
  border-radius: 6px;
  background: var(--el-color-primary-light-9);
  border: 1px solid var(--el-color-primary-light-7);
}
.live-head {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 13px;
}
.live-icon {
  color: var(--el-color-primary);
  font-size: 16px;
}
.live-step {
  flex: 1;
  min-width: 0;
  font-weight: 600;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.live-time {
  flex-shrink: 0;
  white-space: nowrap;
  color: var(--el-text-color-secondary);
  font-size: 12px;
  font-variant-numeric: tabular-nums;
}
.live-time .sep {
  margin: 0 6px;
}
.live-sub {
  font-size: 12px;
}
/* 没有进度可显示时的「仍在进行」动画条 */
.pulse {
  position: relative;
  height: 4px;
  overflow: hidden;
  border-radius: 2px;
  background: var(--el-color-primary-light-8);
}
.pulse span {
  position: absolute;
  top: 0;
  left: -40%;
  width: 40%;
  height: 100%;
  border-radius: 2px;
  background: var(--el-color-primary);
  animation: pulse 1.4s ease-in-out infinite;
}
@keyframes pulse {
  to {
    left: 100%;
  }
}
.idle {
  font-size: 12px;
  color: var(--el-color-warning);
}
.spin {
  animation: spin 1s linear infinite;
}
@keyframes spin {
  to {
    transform: rotate(360deg);
  }
}
.running-tag :deep(.el-tag__content) {
  display: inline-flex;
  align-items: center;
  gap: 3px;
}
.fix-btn {
  margin-top: 6px;
}
.log {
  flex: 1;
  min-height: 200px;
  overflow: auto;
  background: #0f172a;
  color: #e2e8f0;
  border-radius: 6px;
  padding: 10px 12px;
  white-space: pre-wrap;
  word-break: break-all;
  line-height: 1.55;
}
.log .ev-build {
  color: #94a3b8;
}
.log .ev-step {
  color: #7dd3fc;
}
.log .ev-error {
  color: #fca5a5;
}
.log .has-fix {
  margin: 6px 0;
  padding: 8px 10px;
  border-radius: 4px;
  background: rgba(248, 113, 113, 0.08);
  border-left: 3px solid #f87171;
  font-family: var(--el-font-family);
  line-height: 1.7;
}
.log .ev-warn.has-fix {
  background: rgba(252, 211, 77, 0.08);
  border-left-color: #fcd34d;
}
.log .has-fix .fix {
  display: block;
  margin-top: 4px;
  color: #bbf7d0;
  font-family: 'Cascadia Mono', Consolas, monospace;
}
.log .has-fix .tag {
  display: inline-block;
  margin-right: 8px;
  padding: 0 6px;
  border-radius: 3px;
  font-size: 12px;
  line-height: 18px;
  background: rgba(255, 255, 255, 0.12);
  color: #fff;
}
.log .ev-warn {
  color: #fcd34d;
}
.log .ev-ok {
  color: #86efac;
}
.log .ev-rollback {
  color: #d8b4fe;
}
.tip {
  flex-shrink: 0;
}
/* 多行的错误（如本机哪些进程占着要构建的 jar）按行显示 */
.tip :deep(.el-alert__title) {
  white-space: pre-wrap;
}
.result {
  margin-top: 4px;
}
</style>
