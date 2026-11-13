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
  /** 打开切换运行环境（叠在审阅部署上面；prefer：先选中的已有站点的网站目录） */
  switchRuntime: [app: string, target: string, prefer?: string]
}>()

const visible = ref(true)
const targets = ref<string[]>(props.initialTargets?.length ? [...props.initialTargets] : [props.target])
const skipBuild = ref(appSettings.value?.deploy.skipBuild ?? false)
const full = ref(appSettings.value?.deploy.full ?? false)
const noRollback = ref(false)
// 部署到多台时怎么切换：初值来自 ferry.yaml 的 rollout
const ro = props.info.rollout
const batch = ref(Math.max(1, ro?.batch || 1))
const canary = ref((ro?.canary ?? 0) > 0)
const observeMin = ref(Math.round((ro?.observeSec ?? 0) / 60))
const onFailure = ref<'stop' | 'rollback'>(ro?.onFailure === 'rollback' ? 'rollback' : 'stop')
const rolloutText = computed(() => {
  const n = targets.value.length
  const parts: string[] = []
  if (canary.value) parts.push(`先部署 ${targets.value[0]}${observeMin.value ? `，观察 ${observeMin.value} 分钟` : ''}，问你之后再部署其余的`)
  parts.push(batch.value > 1 ? `每次 ${Math.min(batch.value, n)} 台同时切换` : '一台一台切换')
  parts.push(onFailure.value === 'rollback' ? '有一台失败就停下，并把这次已经部署好的几台回滚到部署前的版本' : '有一台失败就停下，已经部署好的保持新版本')
  return parts.join('；')
})
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

// 要上线的改动：和各台服务器上运行的版本比（读不到不影响部署）。
// 勾着的服务器同时读，读过的记着：切换看哪台时不用再等，页面也不会一会儿空、一会儿满
interface TargetReview { data?: AppChange[]; error?: string; loading: boolean }
const reviews = ref<Record<string, TargetReview>>({})
const reviewTarget = ref(props.target)
const reads = useReadRequests()
async function loadChanges(t: string) {
  if (!t || conflicts.value.some((p) => p.target.name === t)) return
  reviews.value[t] = { loading: true }
  try {
    const r = await reads.run('changes:' + t, (s) => readCall<AppChange[]>(['changes', t, ...(props.apps.length ? [props.apps.join(',')] : []), '-c', props.config], s))
    reviews.value[t] = { data: r ?? [], loading: false }
  } catch (e) {
    if (e instanceof ReadCanceled) return
    reviews.value[t] = { error: (e as Error).message, loading: false }
  }
}
watch(targets, (names) => {
  if (!names.includes(reviewTarget.value)) reviewTarget.value = names[0] ?? ''
  prodInput.value = ''
  for (const t of names) if (!reviews.value[t]) loadChanges(t)
}, { deep: true, immediate: true })
const review = computed(() => reviews.value[reviewTarget.value])
const changes = computed(() => review.value?.data)
const changesError = computed(() => review.value?.error ?? '')
const changesLoading = computed(() => !!review.value?.loading)
// 切换按钮上的小点：读取中、读不到、有要注意的（端口被占着、线上有本地没有的提交）、正常
function reviewState(t: string): 'loading' | 'error' | 'warn' | 'ok' {
  const r = reviews.value[t]
  if (!r || r.loading) return 'loading'
  if (r.error) return 'error'
  return (r.data ?? []).some((c) => c.ports?.length || c.warn) ? 'warn' : 'ok'
}
const stateTip = { loading: '正在读取', error: '读不到线上的版本', warn: '有要注意的地方', ok: '' }
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

// 各项在查看的那台服务器上的情况（勾选、加、改「共用」后重新看）；按「服务器 | 应用 | 要保留的」记着，切回来不用再读
const keepReports = ref<Record<string, KeepItemReport[]>>({})
const keepErrors = ref<Record<string, string>>({})
const keepLoading = ref<Record<string, boolean>>({})
const keepReads = useReadRequests()
const keepItems = (app: string) => (keepRows.value[app] ?? []).map((r) => (r.shared ? { path: r.path, shared: true } : { path: r.path }))
const keepCacheKey = (target: string, app: string) => `${target}|${app}|${JSON.stringify(keepItems(app))}`
async function loadKeep() {
  const target = reviewTarget.value
  if (!target) return
  await Promise.all(keepApps.value.map(async (app) => {
    const k = keepCacheKey(target, app)
    if (keepReports.value[k] || keepLoading.value[k]) return
    keepLoading.value[k] = true
    try {
      const r = await keepReads.run(k, (s) => readCall<KeepReport>(['keep', 'scan', target, app, '--keep-files', JSON.stringify(keepItems(app)), '-c', props.config], s))
      keepReports.value[k] = r.items ?? []
      delete keepErrors.value[k]
    } catch (e) {
      if (!(e instanceof ReadCanceled)) keepErrors.value[k] = (e as Error).message
    } finally {
      delete keepLoading.value[k]
    }
  }))
}
const keepKey = computed(() => reviewTarget.value + '|' + JSON.stringify(keepApps.value.map((a) => [a, keepRows.value[a].map((r) => [r.path, r.shared])])))
watch(keepKey, loadKeep, { immediate: true })
onBeforeUnmount(() => { reads.cancelAll(); keepReads.cancelAll() })
const reportFor = (app: string, r: KeepRow) => keepReports.value[keepCacheKey(reviewTarget.value, app)]?.find((x) => x.path === r.path && !!x.shared === !!r.shared)
const keepErrorFor = (app: string) => keepErrors.value[keepCacheKey(reviewTarget.value, app)]
const keepLoadingFor = (app: string) => !!keepLoading.value[keepCacheKey(reviewTarget.value, app)]
const switchRuntime = (app: string, prefer?: string) => emit('switchRuntime', app, reviewTarget.value, prefer)
// 配置改了（在上面叠着的「切换运行环境」里保存了）：读过的都作废，勾着的服务器重新比较
watch(() => props.info, () => {
  reads.cancelAll()
  keepReads.cancelAll()
  reviews.value = {}
  keepReports.value = {}
  keepErrors.value = {}
  for (const t of targets.value) loadChanges(t)
  loadKeep()
})

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
  if (targets.value.length > 1) {
    args.push('--batch', String(batch.value), '--canary', canary.value ? '1' : '0', '--on-failure', onFailure.value)
    if (canary.value) args.push('--observe', `${observeMin.value * 60}s`)
  }
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
  <el-dialog v-model="visible" title="审阅部署" width="880px" top="5vh" append-to-body class="deploy-review" @closed="emit('close')">
    <div class="deploy-summary">
      <div><span>目标服务器</span><strong>{{ targets.length }} <small>台</small></strong></div>
      <div><span>本次应用</span><strong>{{ appList.length }} <small>个</small></strong></div>
      <div><span>执行顺序</span><b>预检 → 构建 → 上传 → 切换</b><b v-if="targets.length > 1" class="muted">{{ batch > 1 ? `每次 ${Math.min(batch, targets.length)} 台同时切换` : '一台一台切换' }}{{ canary ? '，先部署一台' : '' }}</b></div>
    </div>
    <el-form label-width="84px" class="review-form">
      <el-form-item label="目标">
        <el-checkbox-group v-model="targets">
          <el-checkbox v-for="t in info.targets" :key="t.name" :value="t.name">
            {{ t.name }}<el-tag v-if="t.production" size="small" type="danger" effect="plain" class="prod-tag" disable-transitions>生产</el-tag>
          </el-checkbox>
        </el-checkbox-group>
      </el-form-item>
      <el-form-item label="应用">
        <div class="full">
          <div class="apps-line">
            <el-tag v-for="a in appList" :key="a" class="tag" disable-transitions>{{ a }}</el-tag>
          </div>
          <div class="plan-grid">
            <template v-for="p in plans" :key="p.target.name">
              <b>{{ p.target.name }}</b>
              <span class="mono muted">{{ p.target.user }}@{{ p.target.host }}:{{ p.target.port }}</span>
              <span>{{ p.apps.join('、') || '没有可部署应用' }}<span v-if="p.unavailable.length" class="invalid">　不允许部署：{{ p.unavailable.join('、') }}</span></span>
            </template>
          </div>
          <div v-if="conflicts.length" class="invalid small">请调整目标选择：所选应用必须在每台目标的允许列表中。</div>
          <div class="muted small">{{ noRollback ? '某个应用失败后保留失败版本，不自动回滚；后续应用和目标停止部署。' : '按配置顺序逐个部署；某个应用失败会自动回滚它，并且不再部署后面的应用和目标。' }}</div>
        </div>
      </el-form-item>
      <el-form-item label="服务器上">
        <!-- 各台服务器上的情况：切换按钮在最上面，切换时只换下面的内容，按钮不会跟着跳 -->
        <section class="review" aria-label="服务器上的情况">
          <div class="review-head">
            <el-radio-group v-if="targets.length > 1" v-model="reviewTarget" size="small" aria-label="查看哪台服务器">
              <el-radio-button v-for="name in targets" :key="name" :value="name">
                <span class="state-dot" :class="reviewState(name)" :title="stateTip[reviewState(name)]" />{{ name }}
              </el-radio-button>
            </el-radio-group>
            <b v-else-if="reviewTarget" class="review-one">{{ reviewTarget }}</b>
            <span class="muted small review-what">和线上运行的版本比，检查端口</span>
            <el-button v-if="reviewTarget" link type="primary" size="small" :loading="changesLoading" @click="loadChanges(reviewTarget)">重新比较</el-button>
          </div>
          <div class="review-body">
            <div v-if="!targets.length" class="muted small">先选择目标服务器</div>
            <template v-else>
              <div v-if="runtimeRows.length" class="review-sec">
                <div class="sec-label">运行环境</div>
                <div class="changes">
                  <div v-for="c in runtimeRows" :key="'r' + c.app" class="change">
                    <div><b>{{ c.app }}</b>：{{ c.runtime || '按配置运行' }} <el-button link type="primary" size="small" @click="switchRuntime(c.app)">切换运行环境</el-button></div>
                    <div v-for="p in c.ports ?? []" :key="p.port" class="port-busy">
                      <div class="warn small">{{ p.owner }}。照这样部署会在最后切换时失败（原来在用这个端口的程序不受影响）。</div>
                      <div v-for="s in p.sites ?? []" :key="s.root + s.conf" class="small">
                        {{ s.server }}在这个端口上已有站点<span v-if="s.names?.length">（{{ s.names.join(' ') }}）</span>，网站目录 <span class="mono">{{ s.dir || s.root }}</span>
                        <el-button v-if="!s.disabled && s.dir && isStatic(c.app)" link type="primary" size="small" @click="switchRuntime(c.app, s.dir)">改成只换这个站点的文件</el-button>
                        <span v-else-if="s.disabled" class="muted">（{{ s.disabled }}）</span>
                      </div>
                      <div class="muted small">{{ isStatic(c.app) ? '也可以在「切换运行环境」里换一个空闲的端口。' : '先停掉占着端口的程序，或在配置里换一个端口。' }}</div>
                    </div>
                  </div>
                </div>
              </div>
              <div class="review-sec">
                <div class="sec-label">改动</div>
                <div class="changes">
                  <div v-if="changesLoading && !changes" class="loading-line" role="status"><el-icon class="is-loading"><Loading /></el-icon>正在和 {{ reviewTarget }} 上运行的版本比较、检查端口…</div>
                  <div v-else-if="changesError" class="small muted">读不到 {{ reviewTarget }} 上运行的版本：{{ changesError }}。仍可部署，开始后会执行预检。</div>
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
              </div>
              <div class="review-sec">
                <div class="sec-label">保留</div>
                <div class="keep">
                  <div class="muted small">勾选的用服务器上的，不会被包里的替换（服务器上还没有时用包里的）</div>
                  <div v-for="a in keepApps" :key="a" class="keep-app">
                    <div class="keep-head">
                      <b>{{ a }}</b>
                      <el-button link type="primary" size="small" :disabled="!reviewTarget" @click="openPicker(a)">从服务器选…</el-button>
                      <span v-if="keepErrorFor(a)" class="muted small">读不到服务器上的情况：{{ keepErrorFor(a) }}</span>
                    </div>
                    <div v-for="r in keepRows[a]" :key="r.path" class="keep-row">
                      <el-checkbox v-model="r.on"><span class="mono">{{ r.path }}</span></el-checkbox>
                      <el-tooltip :show-after="500" content="共用：放在应用的 data/ 下只有一份，每个版本链接过去，回滚时不动（上传目录等）。不勾：从运行中的版本复制过去，回滚时跟着版本走" placement="top">
                        <el-checkbox v-model="r.shared" size="small" :disabled="!r.on || hasGlob(r.path)">共用</el-checkbox>
                      </el-tooltip>
                      <span v-if="!r.on" class="muted small">这次用包里的</span>
                      <span v-else-if="reportFor(a, r)" class="small" :class="reportFor(a, r)?.differs ? 'warn' : 'muted'">{{ reportFor(a, r)?.note }}</span>
                      <span v-else-if="keepLoadingFor(a)" class="muted small"><el-icon class="is-loading"><Loading /></el-icon> 正在看服务器上的…</span>
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
              </div>
            </template>
          </div>
        </section>
      </el-form-item>
      <el-form-item v-if="targets.length > 1" label="部署方式">
        <div class="rollout">
          <div class="rollout-row">
            <el-checkbox v-model="canary">先部署一台（{{ targets[0] }}），确认没问题再部署其余的</el-checkbox>
          </div>
          <div v-if="canary" class="rollout-row indent">
            <span class="small">上线后观察</span>
            <el-input-number v-model="observeMin" :min="0" :max="60" size="small" controls-position="right" class="num" aria-label="观察几分钟" />
            <span class="small">分钟</span>
            <span class="muted small">这期间应用一直要在运行、健康检查要通过；0 = 不观察，上线后马上问你</span>
          </div>
          <div class="rollout-row">
            <span class="small">每次同时切换</span>
            <el-input-number v-model="batch" :min="1" :max="Math.max(1, targets.length)" size="small" controls-position="right" class="num" aria-label="每次同时切换几台" />
            <span class="small">台</span>
            <span class="rollout-gap" />
            <span class="small">有一台失败时</span>
            <el-radio-group v-model="onFailure" size="small">
              <el-radio-button value="stop">停下</el-radio-button>
              <el-radio-button value="rollback">回滚已部署的</el-radio-button>
            </el-radio-group>
          </div>
          <div class="muted small">{{ rolloutText }}</div>
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
.rollout { display: flex; flex-direction: column; gap: 6px; width: 100%; }
.rollout-row { display: flex; align-items: center; flex-wrap: wrap; gap: 8px; }
.rollout-row .num { width: 96px; }
.rollout-row.indent { padding-left: 24px; }
.advanced-options summary { cursor: pointer; color: var(--el-text-color-secondary); margin-bottom: 8px; }
.full { width: 100%; }
.apps-line { margin-bottom: 4px; }
.plan-grid { display: grid; grid-template-columns: max-content max-content 1fr; gap: 6px 16px; align-items: baseline; font-size: 12px; padding: 8px 12px; margin-bottom: 4px; border-radius: 8px; background: var(--el-fill-color-lighter); }
.review { width: 100%; border: 1px solid var(--ferry-stroke); border-radius: 10px; overflow: hidden; }
.review-head { display: flex; align-items: center; flex-wrap: wrap; gap: 8px 12px; padding: 8px 12px; background: var(--el-fill-color-light); border-bottom: 1px solid var(--ferry-stroke); }
.review-what { margin-left: auto; }
.review-one { font-size: 13px; }
.review-body { padding: 4px 12px 10px; min-height: 150px; }
.review-sec { display: grid; grid-template-columns: 64px 1fr; gap: 12px; padding: 8px 0; }
.review-sec + .review-sec { border-top: 1px dashed var(--ferry-stroke); }
.sec-label { font-size: 12px; color: var(--el-text-color-secondary); line-height: 1.6; padding-top: 1px; }
.state-dot { display: inline-block; width: 7px; height: 7px; border-radius: 50%; margin-right: 6px; vertical-align: 1px; background: var(--el-color-success); }
.state-dot.loading { background: var(--el-text-color-placeholder); animation: dot-pulse 1s ease-in-out infinite alternate; }
.state-dot.error { background: var(--el-text-color-secondary); }
.state-dot.warn { background: var(--el-color-warning); }
@keyframes dot-pulse { from { opacity: .3; } to { opacity: 1; } }
.rollout-gap { width: 16px; }
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
