<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { call, type AppChange, type Inspect, type KeepItem, type KeepItemReport, type KeepReport } from '../ferry'
import { readCall, ReadCanceled, useReadRequests } from '../readRequests'
import { appSettings, startTask } from '../state'
import { deploymentTargets } from '../deploymentReview'
import KeepPicker from './KeepPicker.vue'
import { Loading } from '@element-plus/icons-vue'

const props = defineProps<{ config: string; info: Inspect; target: string; apps: string[]; initialTargets?: string[] }>()
const emit = defineEmits<{
  close: []
  started: [id: number]
  finished: []
  /** 去切换运行环境（prefer：先选中的已有站点的网站目录） */
  switchRuntime: [app: string, target: string, prefer?: string]
}>()

const visible = ref(true)
const targets = ref<string[]>(props.initialTargets?.length ? [...props.initialTargets] : [props.target])
const skipBuild = ref(appSettings.value?.deploy.skipBuild ?? false)
const full = ref(appSettings.value?.deploy.full ?? false)
const noRollback = ref(false)
// 选了生产服务器：输入服务器名才能开始（多台时用逗号连起来）
const prodTargets = computed(() => props.info.targets.filter((t) => t.production && targets.value.includes(t.name)).map((t) => t.name))
const prodInput = ref('')
const prodOK = computed(() => !prodTargets.value.length || prodInput.value.replace(/[\s，]/g, ',').split(',').filter(Boolean).join(',') === prodTargets.value.join(','))

// 要部署的应用：勾选的，或目标允许的全部
const plans = computed(() => deploymentTargets(props.info, targets.value, props.apps))
const appList = computed(() => [...new Set(plans.value.flatMap((p) => p.apps))])
const conflicts = computed(() => plans.value.filter((p) => p.unavailable.length || !p.apps.length))
const starting = ref(false)
const canStart = computed(() => targets.value.length > 0 && appList.value.length > 0 && !conflicts.value.length && prodOK.value && !starting.value)
const builds = computed(() => {
  const set = new Set<string>()
  for (const a of props.info.apps) if (appList.value.includes(a.name) && a.build) set.add(a.build)
  return [...set]
})

// 要上线的改动：和这台服务器上运行的版本比（读不到不影响部署）
const changes = ref<AppChange[]>()
const changesError = ref('')
const changesLoading = ref(false)
const reviewTarget = ref(props.target)
const reads = useReadRequests()
let compareSeq = 0
watch(targets, (names) => {
  if (!names.includes(reviewTarget.value)) reviewTarget.value = names[0] ?? ''
  prodInput.value = ''
}, { deep: true })
async function loadChanges() {
  const seq = ++compareSeq
  reads.cancelAll()
  changes.value = undefined
  changesError.value = ''
  changesLoading.value = false
  if (!reviewTarget.value || conflicts.value.some((p) => p.target.name === reviewTarget.value)) return
  changesLoading.value = true
  try {
    const r = await reads.run('changes', (s) => readCall<AppChange[]>(['changes', reviewTarget.value, ...(props.apps.length ? [props.apps.join(',')] : []), '-c', props.config], s))
    if (seq !== compareSeq) return
    changes.value = r ?? []
  } catch (e) {
    if (seq !== compareSeq || e instanceof ReadCanceled) return
    changesError.value = (e as Error).message
  } finally {
    if (seq === compareSeq) changesLoading.value = false
  }
}
watch(reviewTarget, loadChanges, { immediate: true })
onBeforeUnmount(() => { compareSeq++; reads.cancelAll() })
// 线上版本相同的应用写在一起
const changeGroups = computed(() => {
  const groups: { apps: string[]; c: AppChange }[] = []
  for (const c of changes.value ?? []) {
    const key = (x: AppChange) => `${x.online?.id ?? ''}|${x.online?.commit ?? ''}|${x.note ?? ''}`
    const g = groups.find((g) => key(g.c) === key(c))
    if (g) g.apps.push(c.app)
    else groups.push({ apps: [c.app], c })
  }
  return groups
})
// 按什么运行、端口被占着的（第一次部署时服务器上原来就有程序在用这个端口最常见）
const runtimeRows = computed(() => (changes.value ?? []).filter((c) => c.runtime || c.ports?.length))
const busyCount = computed(() => runtimeRows.value.reduce((n, c) => n + (c.ports?.length ?? 0), 0))
const isStatic = (app: string) => props.info.apps.find((a) => a.name === app)?.type === 'static'
const short = (h?: string) => (h ? h.slice(0, 8) : '-')
const when = (t?: string) => (t ? new Date(t).toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' }) : '')

// 部署时保留服务器上的（artifact.keep）：配置里的默认勾上，可以这次不保留、从服务器上加
type KeepRow = KeepItem & { on: boolean; added?: boolean }
const appInfo = (name: string) => props.info.apps.find((a) => a.name === name)
const keepRows = ref<Record<string, KeepRow[]>>(Object.fromEntries(props.info.apps.map((a) => [a.name, (a.keep ?? []).map((k) => ({ path: k.path, shared: !!k.shared, on: true }))])))
const keepApps = computed(() => appList.value.filter((a) => keepRows.value[a]?.length))
const otherApps = computed(() => appList.value.filter((a) => !keepRows.value[a]?.length))
const addApp = ref('')
watch(otherApps, (list) => { if (!list.includes(addApp.value)) addApp.value = list[0] ?? '' }, { immediate: true })
const chosen = (app: string): KeepItem[] => (keepRows.value[app] ?? []).filter((r) => r.on).map((r) => (r.shared ? { path: r.path, shared: true } : { path: r.path }))
// 和 ferry.yaml 里的不一样（这次要用 --keep-files 告诉部署）
function keepEdited(app: string) {
  const yaml = appInfo(app)?.keep ?? []
  const now = chosen(app)
  return now.length !== yaml.length || now.some((k, i) => k.path !== yaml[i].path || !!k.shared !== !!yaml[i].shared)
}
const editedApps = computed(() => appList.value.filter(keepEdited))
const saveKeep = ref(false)
const hasGlob = (p: string) => /[*?[]/.test(p)

// 各项在比较的那台服务器上的情况（勾选、加、改「共用」后重新看）
const keepReports = ref<Record<string, KeepItemReport[]>>({})
const keepErrors = ref<Record<string, string>>({})
const keepReads = useReadRequests()
let keepSeq = 0
async function loadKeep() {
  const seq = ++keepSeq
  keepReads.cancelAll()
  keepErrors.value = {}
  const target = reviewTarget.value
  if (!target) return
  await Promise.all(keepApps.value.map(async (app) => {
    const items = (keepRows.value[app] ?? []).map((r) => (r.shared ? { path: r.path, shared: true } : { path: r.path }))
    try {
      const r = await keepReads.run(app, (s) => readCall<KeepReport>(['keep', 'scan', target, app, '--keep-files', JSON.stringify(items), '-c', props.config], s))
      if (seq === keepSeq) keepReports.value[app] = r.items ?? []
    } catch (e) {
      if (seq !== keepSeq || e instanceof ReadCanceled) return
      keepErrors.value[app] = (e as Error).message
    }
  }))
}
const keepKey = computed(() => reviewTarget.value + '|' + JSON.stringify(keepApps.value.map((a) => [a, keepRows.value[a].map((r) => [r.path, r.shared])])))
watch(keepKey, loadKeep, { immediate: true })
const reportFor = (app: string, r: KeepRow) => keepReports.value[app]?.find((x) => x.path === r.path && !!x.shared === !!r.shared)

const picker = ref<{ app: string; type: string; program?: string }>()
function openPicker(app: string) {
  const a = appInfo(app)
  if (!a || !reviewTarget.value) return
  const program = a.type === 'java' || a.type === 'binary' ? a.artifact.split(/[\\/]/).pop() : undefined
  picker.value = { app, type: a.type, program }
}
function onPick(app: string, items: KeepItem[]) {
  const rows = (keepRows.value[app] ??= [])
  for (const it of items) {
    const old = rows.find((r) => r.path === it.path)
    if (old) old.on = true
    else rows.push({ path: it.path, shared: !!it.shared, on: true, added: true })
  }
}
function removeRow(app: string, r: KeepRow) {
  keepRows.value[app] = keepRows.value[app].filter((x) => x !== r)
}

async function start() {
  if (!canStart.value) return
  starting.value = true
  const args = ['deploy', targets.value.join(','), ...(props.apps.length ? [props.apps.join(',')] : []), '-c', props.config, '--yes']
  if (skipBuild.value) args.push('--skip-build')
  if (full.value) args.push('--full')
  if (noRollback.value) args.push('--no-rollback')
  const edited = editedApps.value
  if (edited.length) {
    args.push('--keep-files', JSON.stringify(Object.fromEntries(edited.map((a) => [a, chosen(a)]))))
    if (saveKeep.value) {
      try {
        for (const a of edited) await call(['keep', 'set', a, JSON.stringify(chosen(a)), '-c', props.config])
      } catch (e) {
        ElMessage.error(`写进 ferry.yaml 失败：${(e as Error).message}`)
        starting.value = false
        return
      }
    }
  }
  const task = startTask(`部署 ${targets.value.join('、')}：${appList.value.join('、')}`, args, () => emit('finished'))
  emit('started', task.id)
  visible.value = false
}
</script>

<template>
  <el-dialog v-model="visible" title="审阅部署" width="820px" top="6vh" append-to-body @closed="emit('close')">
    <div class="deploy-summary"><div><span>目标服务器</span><strong>{{ targets.length }} <small>台</small></strong></div><div><span>本次应用</span><strong>{{ appList.length }} <small>个</small></strong></div><div><span>执行顺序</span><b>预检 → 构建 → 上传 → 切换</b></div></div>
    <el-form label-width="90px">
      <el-form-item label="目标">
        <el-checkbox-group v-model="targets">
          <el-checkbox v-for="t in info.targets" :key="t.name" :value="t.name">
            {{ t.name }}<el-tag v-if="t.production" size="small" type="danger" effect="plain" class="prod-tag" disable-transitions>生产</el-tag>
          </el-checkbox>
        </el-checkbox-group>
      </el-form-item>
      <el-form-item label="应用">
        <div>
          <el-tag v-for="a in appList" :key="a" class="tag">{{ a }}</el-tag>
          <div class="muted small">{{ noRollback ? '某个应用失败后保留失败版本，不自动回滚；后续应用和目标停止部署。' : '按配置顺序逐个部署；某个应用失败会自动回滚它，并且不再部署后面的应用和目标。' }}</div>
          <div v-for="p in plans" :key="p.target.name" class="target-plan"><b>{{ p.target.name }}</b><span class="mono muted">{{ p.target.user }}@{{ p.target.host }}:{{ p.target.port }}</span><span>{{ p.apps.join('、') || '没有可部署应用' }}</span><span v-if="p.unavailable.length" class="invalid">不允许部署：{{ p.unavailable.join('、') }}</span></div>
          <div v-if="conflicts.length" class="invalid small">请调整目标选择：所选应用必须在每台目标的允许列表中。</div>
        </div>
      </el-form-item>
      <el-form-item v-if="runtimeRows.length" label="运行环境">
        <div class="changes">
          <div v-if="targets.length > 1" class="muted small">下面是 {{ reviewTarget }} 上的情况</div>
          <div v-for="c in runtimeRows" :key="'r' + c.app" class="change">
            <div><b>{{ c.app }}</b>：{{ c.runtime || '按配置运行' }} <el-button link type="primary" size="small" @click="emit('switchRuntime', c.app, reviewTarget)">切换运行环境</el-button></div>
            <div v-for="p in c.ports ?? []" :key="p.port" class="port-busy">
              <div class="warn small">{{ p.owner }}。照这样部署会在最后切换时失败（原来在用这个端口的程序不受影响）。</div>
              <div v-for="s in p.sites ?? []" :key="s.root + s.conf" class="small">
                {{ s.server }}在这个端口上已有站点<span v-if="s.names?.length">（{{ s.names.join(' ') }}）</span>，网站目录 <span class="mono">{{ s.dir || s.root }}</span>
                <el-button v-if="!s.disabled && s.dir && isStatic(c.app)" link type="primary" size="small" @click="emit('switchRuntime', c.app, reviewTarget, s.dir)">改成只换这个站点的文件</el-button>
                <span v-else-if="s.disabled" class="muted">（{{ s.disabled }}）</span>
              </div>
              <div class="muted small">{{ isStatic(c.app) ? '也可以在「切换运行环境」里换一个空闲的端口。' : '先停掉占着端口的程序，或在配置里换一个端口。' }}</div>
            </div>
          </div>
        </div>
      </el-form-item>
      <el-form-item label="改动">
        <div class="changes">
          <el-radio-group v-if="targets.length > 1" v-model="reviewTarget" size="small" class="compare-targets" aria-label="比较服务器"><el-radio-button v-for="name in targets" :key="name" :value="name">{{ name }}</el-radio-button></el-radio-group>
          <div v-if="!targets.length" class="muted small">先选择目标服务器</div>
          <div v-else-if="changesLoading" class="loading-line" role="status"><el-icon class="is-loading"><Loading /></el-icon>正在和 {{ reviewTarget }} 上运行的版本比较、检查端口…</div>
          <div v-else-if="changesError" class="small"><span class="muted">读不到 {{ reviewTarget }} 上运行的版本：{{ changesError }}。仍可部署，开始后会执行预检。</span> <el-button link type="primary" size="small" @click="loadChanges">重新比较</el-button></div>
          <div v-else-if="changes?.length === 0" class="muted small">{{ reviewTarget }} 暂无可比较的版本信息</div>
          <div v-for="g in changeGroups" :key="g.apps.join()" class="change">
            <div>
              <b>{{ g.apps.join('、') }}</b>
              <template v-if="g.c.online">
                ：线上 <span class="mono">{{ short(g.c.online.commit) }}</span>
                <span class="muted small">（{{ when(g.c.online.deployedAt) }} {{ g.c.online.deployedBy }}）</span>
                → 本次 <span class="mono">{{ short(g.c.local.commit) }}{{ g.c.local.dirty ? '*' : '' }}</span>
                <span v-if="g.c.ahead">，新增 {{ g.c.ahead }} 个提交</span>
              </template>
            </div>
            <ul v-if="g.c.commits?.length" class="commits">
              <li v-for="x in g.c.commits.slice(0, 8)" :key="x.hash">
                <span class="mono">{{ x.hash }}</span> {{ x.subject }} <span class="muted small">{{ x.author }} · {{ x.date }}</span>
              </li>
              <li v-if="g.c.ahead > 8" class="muted small">…还有 {{ g.c.ahead - 8 }} 个</li>
            </ul>
            <div v-if="g.c.note" class="small" :class="g.c.warn ? 'warn' : 'muted'">{{ g.c.note }}</div>
          </div>
          <!-- 配置、附带文件每个应用不一样，分开列 -->
          <template v-for="c in changes ?? []" :key="'d' + c.app">
            <div v-if="c.unitDiff?.length" class="change">
              <b>{{ c.app }}</b> 的 systemd 单元有变化：
              <pre class="diff mono"><span v-for="(l, i) in c.unitDiff" :key="i" :class="l.startsWith('+') ? 'add' : 'del'">{{ l }}{{ '\n' }}</span></pre>
            </div>
            <div v-if="c.filesAdded?.length || c.filesChanged?.length || c.filesRemoved?.length" class="change">
              <b>{{ c.app }}</b> 的附带文件：
              <span v-if="c.filesAdded?.length">新增 <span class="mono">{{ c.filesAdded.join('、') }}</span>；</span>
              <span v-if="c.filesChanged?.length">改了 <span class="mono">{{ c.filesChanged.join('、') }}</span>；</span>
              <span v-if="c.filesRemoved?.length">去掉 <span class="mono">{{ c.filesRemoved.join('、') }}</span></span>
            </div>
          </template>
        </div>
      </el-form-item>
      <el-form-item label="保留">
        <div class="keep">
          <div class="muted small">勾选的用服务器上的，不会被包里的替换（服务器上还没有时用包里的）<template v-if="targets.length > 1">；下面是 {{ reviewTarget }} 上的情况</template></div>
          <div v-for="a in keepApps" :key="a" class="keep-app">
            <div class="keep-head">
              <b>{{ a }}</b>
              <el-button link type="primary" size="small" :disabled="!reviewTarget" @click="openPicker(a)">从服务器选…</el-button>
              <span v-if="keepErrors[a]" class="muted small">读不到服务器上的情况：{{ keepErrors[a] }}</span>
            </div>
            <div v-for="r in keepRows[a]" :key="r.path" class="keep-row">
              <el-checkbox v-model="r.on"><span class="mono">{{ r.path }}</span></el-checkbox>
              <el-tooltip :show-after="500" content="共用：放在应用的 data/ 下只有一份，每个版本链接过去，回滚时不动（上传目录等）。不勾：从运行中的版本复制过去，回滚时跟着版本走" placement="top">
                <el-checkbox v-model="r.shared" size="small" :disabled="!r.on || hasGlob(r.path)">共用</el-checkbox>
              </el-tooltip>
              <span v-if="!r.on" class="muted small">这次用包里的</span>
              <span v-else-if="reportFor(a, r)" class="small" :class="reportFor(a, r)?.differs ? 'warn' : 'muted'">{{ reportFor(a, r)?.note }}</span>
              <span v-else-if="keepReads.loading.value[a]" class="muted small"><el-icon class="is-loading"><Loading /></el-icon> 正在看服务器上的…</span>
              <el-button v-if="r.added" link size="small" @click="removeRow(a, r)">去掉</el-button>
            </div>
          </div>
          <div v-if="otherApps.length" class="keep-add">
            <span class="muted small">{{ keepApps.length ? '其他应用' : '没有要保留的：服务器上的文件都换成包里的' }}</span>
            <el-select v-if="otherApps.length > 1" v-model="addApp" size="small" class="keep-app-select" aria-label="选应用"><el-option v-for="a in otherApps" :key="a" :label="a" :value="a" /></el-select>
            <el-button link type="primary" size="small" :disabled="!reviewTarget || !addApp" @click="openPicker(addApp)">从服务器选要保留的{{ otherApps.length === 1 ? `（${addApp}）` : '' }}…</el-button>
          </div>
          <el-checkbox v-if="editedApps.length" v-model="saveKeep" size="small">记到 ferry.yaml（以后部署都这样；不勾只这一次）</el-checkbox>
        </div>
      </el-form-item>
      <el-form-item label="构建">
        <div>
          <el-checkbox v-model="skipBuild">跳过构建，直接使用现有产物</el-checkbox>
          <div v-if="!skipBuild && builds.length" class="muted small">将执行构建：{{ builds.join('、') }}</div>
        </div>
      </el-form-item>
      <el-form-item label="选项">
        <details class="advanced-options"><summary>高级选项</summary><div class="col">
          <el-checkbox v-model="full">全量上传（不使用增量）</el-checkbox>
          <el-checkbox v-model="noRollback">失败时不回滚（保留失败版本排查用）</el-checkbox>
        </div></details>
      </el-form-item>
      <el-form-item v-if="prodTargets.length" label="生产服务器">
        <div class="prod">
          <div class="prod-tip">{{ prodTargets.join('、') }} 是生产服务器，部署会影响线上业务。输入服务器名<span class="mono">「{{ prodTargets.join(',') }}」</span>确认：</div>
          <el-input v-model="prodInput" class="mono" :placeholder="prodTargets.join(',')" />
        </div>
      </el-form-item>
    </el-form>
    <template #footer>
      <span v-if="busyCount" class="warn small busy-hint">{{ reviewTarget }} 上有 {{ busyCount }} 个端口被别的程序占着（见上面「运行环境」）</span>
      <el-button @click="visible = false">取消</el-button>
      <el-button :type="prodTargets.length ? 'danger' : 'primary'" :disabled="!canStart" :loading="starting" @click="start">部署到 {{ targets.length }} 台服务器</el-button>
    </template>
  </el-dialog>
  <KeepPicker
    v-if="picker"
    :config="config"
    :target="reviewTarget"
    :app="picker.app"
    :type="picker.type"
    :program="picker.program"
    :selected="(keepRows[picker.app] ?? []).map((r) => r.path)"
    @pick="(items) => onPick(picker!.app, items)"
    @close="picker = undefined"
  />
</template>

<style scoped>
.deploy-summary { display: grid; grid-template-columns: 1fr 1fr 2fr; gap: 20px; padding: 18px; background: var(--el-fill-color-light); border: 1px solid var(--ferry-stroke); border-radius: 10px; margin-bottom: 24px; }
.deploy-summary > div { display: flex; flex-direction: column; gap: 8px; }
.deploy-summary span { font-size: 11px; color: var(--el-text-color-secondary); }
.deploy-summary strong { font-size: 25px; color: var(--el-color-primary); }
.deploy-summary small { font-size: 12px; font-weight: 400; }
.deploy-summary b { font-size: 12px; font-weight: 500; line-height: 1.8; }
.advanced-options summary { cursor: pointer; color: var(--el-text-color-secondary); margin-bottom: 8px; }
.compare-targets { margin-bottom: 10px; }
.target-plan { display: flex; flex-wrap: wrap; gap: 4px 12px; padding: 8px 0; font-size: 12px; border-top: 1px solid var(--ferry-stroke); margin-top: 8px; }
.invalid { color: var(--el-color-danger); }
.tag {
  margin: 0 6px 6px 0;
}
.small {
  font-size: 12px;
  line-height: 1.5;
}
.col {
  display: flex;
  flex-direction: column;
}
.prod {
  width: 100%;
}
.changes {
  width: 100%;
  font-size: 13px;
  line-height: 1.6;
}
.change + .change {
  margin-top: 6px;
}
.commits {
  margin: 2px 0 0;
  padding-left: 18px;
  max-height: 160px;
  overflow-y: auto;
}
.warn {
  color: var(--el-color-warning);
}
.loading-line {
  display: flex;
  align-items: center;
  gap: 6px;
  color: var(--el-color-primary);
}
.port-busy {
  margin: 2px 0 0 12px;
}
.busy-hint {
  margin-right: 12px;
}
.diff {
  margin: 2px 0 0;
  padding: 4px 8px;
  font-size: 12px;
  max-height: 140px;
  overflow: auto;
  background: var(--el-fill-color-lighter);
  border-radius: 4px;
  white-space: pre-wrap;
}
.diff .add {
  color: var(--el-color-success);
}
.diff .del {
  color: var(--el-color-danger);
}
.prod-tip {
  font-size: 12px;
  line-height: 1.6;
  color: var(--el-color-danger);
  margin-bottom: 6px;
}
.prod-tag {
  margin-left: 6px;
}
.keep {
  width: 100%;
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.keep-app + .keep-app,
.keep-add {
  margin-top: 4px;
}
.keep-head {
  display: flex;
  align-items: center;
  gap: 10px;
  font-size: 13px;
}
.keep-row {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 2px 12px;
  padding-left: 12px;
  line-height: 1.6;
}
.keep-row :deep(.el-checkbox) {
  margin-right: 0;
  height: 24px;
}
.keep-add {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
}
.keep-app-select {
  width: 140px;
}
</style>
