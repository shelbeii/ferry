<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { ferry, type AppChange, type Inspect, type Running } from '../ferry'
import { appSettings, startTask } from '../state'
import { deploymentTargets } from '../deploymentReview'

const props = defineProps<{ config: string; info: Inspect; target: string; apps: string[] }>()
const emit = defineEmits<{ close: []; started: [id: number]; finished: [] }>()

const visible = ref(true)
const targets = ref<string[]>([props.target])
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
let comparing: Running<AppChange[]> | undefined
let compareSeq = 0
watch(targets, (names) => {
  if (!names.includes(reviewTarget.value)) reviewTarget.value = names[0] ?? ''
  prodInput.value = ''
}, { deep: true })
async function loadChanges() {
  const seq = ++compareSeq
  comparing?.kill()
  changes.value = undefined
  changesError.value = ''
  changesLoading.value = false
  if (!reviewTarget.value || conflicts.value.some((p) => p.target.name === reviewTarget.value)) return
  changesLoading.value = true
  const proc = ferry<AppChange[]>(['changes', reviewTarget.value, ...(props.apps.length ? [props.apps.join(',')] : []), '-c', props.config])
  comparing = proc
  try {
    const r = await proc.done
    if (seq !== compareSeq) return
    if (!r.ok) throw new Error(r.error ?? '比较失败')
    changes.value = r.data ?? []
  } catch (e) {
    if (seq !== compareSeq) return
    changesError.value = (e as Error).message
  } finally {
    if (seq === compareSeq) changesLoading.value = false
  }
}
watch(reviewTarget, loadChanges, { immediate: true })
onBeforeUnmount(() => { compareSeq++; comparing?.kill() })
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
const short = (h?: string) => (h ? h.slice(0, 8) : '-')
const when = (t?: string) => (t ? new Date(t).toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' }) : '')

function start() {
  if (!canStart.value) return
  starting.value = true
  const args = ['deploy', targets.value.join(','), ...(props.apps.length ? [props.apps.join(',')] : []), '-c', props.config, '--yes']
  if (skipBuild.value) args.push('--skip-build')
  if (full.value) args.push('--full')
  if (noRollback.value) args.push('--no-rollback')
  const task = startTask(`部署 ${targets.value.join('、')}：${appList.value.join('、')}`, args, () => emit('finished'))
  emit('started', task.id)
  visible.value = false
}
</script>

<template>
  <el-dialog v-model="visible" title="审阅部署" width="820px" top="6vh" @closed="emit('close')">
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
      <el-form-item label="改动">
        <div class="changes">
          <el-radio-group v-if="targets.length > 1" v-model="reviewTarget" size="small" class="compare-targets" aria-label="比较服务器"><el-radio-button v-for="name in targets" :key="name" :value="name">{{ name }}</el-radio-button></el-radio-group>
          <div v-if="!targets.length" class="muted small">先选择目标服务器</div>
          <div v-else-if="changesLoading" class="muted small" role="status">正在和 {{ reviewTarget }} 上运行的版本比较…</div>
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
      <el-button @click="visible = false">取消</el-button>
      <el-button :type="prodTargets.length ? 'danger' : 'primary'" :disabled="!canStart" :loading="starting" @click="start">部署到 {{ targets.length }} 台服务器</el-button>
    </template>
  </el-dialog>
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
</style>
