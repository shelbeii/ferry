<script setup lang="ts">
import { computed, defineAsyncComponent, onBeforeUnmount, onMounted, ref } from 'vue'
import { CopyDocument, FolderOpened, Refresh } from '@element-plus/icons-vue'
import { ElMessage } from 'element-plus'
import { time, type AppInfo } from '../ferry'
import { readCall, ReadCanceled, useReadRequests } from '../readRequests'
import LoadingBlock from './LoadingBlock.vue'
import RowActions from './RowActions.vue'
import type { DeploymentRequest } from '../editor/appSetup'
const RuntimeSwitchDialog=defineAsyncComponent(()=>import('./RuntimeSwitchDialog.vue'))
const switching=ref(false)

const props = defineProps<{ config: string; target: string; app: string }>()
const emit = defineEmits<{ close: []; location: [path: string]; saved: [deployment?:DeploymentRequest]; versions: [] }>()
const visible = ref(true)
const loading = ref(true)
const error = ref('')
const data = ref<AppInfo>()
const query = ref('')
const reads = useReadRequests()
let sequence = 0
let disposed = false

async function load() {
  const seq = ++sequence
  reads.cancelAll()
  loading.value = true
  error.value = ''
  try {
    const result = await reads.run('details', (signal) => readCall<AppInfo>(['app-info', props.target, props.app, '-c', props.config], signal))
    if (disposed || seq !== sequence) return
    if (!result) throw new Error('服务器没有返回运行详情')
    data.value = result
  } catch (e) {
    if (!(e instanceof ReadCanceled) && !disposed && seq === sequence) error.value = (e as Error).message
  } finally {
    if (!disposed && seq === sequence) loading.value = false
  }
}
onMounted(load)
onBeforeUnmount(() => { disposed = true; sequence++; reads.cancelAll() })
const typeNames: Record<string, string> = { java: 'Java（JAR）', binary: '原生程序', static: '前端静态站点', python: 'Python 服务', node: 'Node.js 服务', dotnet: '.NET 服务' }
const state = computed(() => {
  const s = data.value?.status
  if (!s) return ''
  if (s.problem) return s.problem
  if (s.active === 'active') return s.healthy === false ? '运行中，健康检查未通过' : '运行中'
  if (s.active === 'failed') return '运行失败'
  if (s.active) return '已停止'
  return s.current ? '已发布' : '未部署'
})
const sections = computed(() => {
  const q = query.value.trim().toLowerCase()
  return (data.value?.sections ?? []).map((section) => ({ ...section, fields: section.fields.filter((field) => !q || `${section.title} ${field.label} ${field.value}`.toLowerCase().includes(q)) })).filter((section) => section.fields.length)
})
async function copy() {
  if (!data.value) return
  const text = [`${data.value.name} · ${props.target}`, `采集时间：${time(data.value.collectedAt)}`, `状态：${state.value}`,
    ...data.value.sections.flatMap((section) => [`\n${section.title}`, ...section.fields.map((field) => `${field.label}：${field.value}`)]),
    ...(data.value.notes ?? []).map((note) => `说明：${note}`)].join('\n')
  try { await navigator.clipboard.writeText(text); ElMessage.success('已复制运行详情') }
  catch { ElMessage.warning('无法写入剪贴板，可以选中详情中的文字复制') }
}
function open(path: string) { emit('location', path) }
</script>

<template>
  <el-dialog v-model="visible" :title="`${app} 的运行情况（${target}）`" width="min(1080px, 94vw)" top="4vh" append-to-body class="app-info-dialog" @closed="emit('close')">
    <div class="app-info-content">
      <div class="info-toolbar">
        <el-input v-model="query" placeholder="搜索运行环境、路径或参数" clearable aria-label="搜索运行详情" />
        <el-button :icon="Refresh" :loading="loading" @click="load">刷新</el-button>
        <el-button :icon="CopyDocument" :disabled="!data || loading" @click="copy">复制详情</el-button>
        <RowActions :actions="[{ id: 'runtime', label: '切换运行环境', run: () => { switching = true }, disabled: !data || loading }]" :label="`${app}运行情况`" :inline-count="0" />
      </div>
      <el-alert v-if="error" type="error" :title="error" :closable="false" show-icon class="info-error" />
      <LoadingBlock v-if="loading && !data" :text="`正在读取 ${target} 上 ${app} 的进程、运行环境和部署位置…`" :rows="7" min-height="340px" />
      <div v-if="data" class="info-scroll" :aria-busy="loading">
        <div class="info-summary">
          <div><strong>{{ typeNames[data.type] ?? data.type }}</strong><el-tag :type="data.status.active === 'active' && data.status.healthy !== false && !data.status.problem ? 'success' : 'info'" effect="plain">{{ state }}</el-tag></div>
          <span class="muted small">采集于 {{ time(data.collectedAt) }}<span v-if="loading"> · 刷新中…</span><span v-else-if="error"> · 刷新失败，以下为上次采集结果</span></span>
        </div>
        <div v-if="data.notes?.length" class="info-notes"><p v-for="(note, index) in data.notes" :key="index">{{ note }}</p></div>
        <section v-for="section in sections" :key="section.id" class="info-section">
          <h3>{{ section.title }}</h3>
          <dl>
            <div v-for="(field, index) in section.fields" :key="`${field.label}-${index}`" class="info-field">
              <dt>{{ field.label }}</dt>
              <dd><span class="mono value">{{ field.value }}</span><el-button v-if="field.path" link type="primary" :icon="FolderOpened" :aria-label="`查看${field.label}的位置`" @click="open(field.value)">查看位置</el-button></dd>
            </div>
          </dl>
        </section>
        <el-empty v-if="!sections.length" description="没有匹配的信息" :image-size="60" />
      </div>
    </div>
    <template #footer><span class="muted small info-tip">详情按需采集，停止的服务只显示已确认的配置与位置。</span><el-button @click="visible = false">关闭</el-button></template>
  </el-dialog>
  <RuntimeSwitchDialog v-if="switching" :config="config" :target="target" :app="app" @close="switching=false" @saved="(request?:DeploymentRequest)=>{emit('saved',request);visible=false}" @versions="emit('versions')" />
</template>

<style scoped>
:global(.app-info-dialog) { height: min(900px, 92dvh); }
:global(.app-info-dialog .el-dialog__body) { display: flex; flex-direction: column; overflow: hidden; }
.app-info-content { display: flex; flex: 1; flex-direction: column; min-height: 0; }
.info-toolbar { display: flex; flex-wrap: wrap; flex-shrink: 0; gap: 10px; margin-bottom: 14px; }
.info-toolbar > .el-input { flex: 1; min-width: 120px; }
.info-toolbar .el-button + .el-button { margin-left: 0; }
.info-scroll { flex: 1; overflow-y: auto; min-height: 0; padding-right: 8px; }
.info-summary, .info-summary > div { display: flex; align-items: center; gap: 12px; flex-wrap: wrap; }
.info-summary { justify-content: space-between; margin-bottom: 14px; }
.info-notes { background: var(--el-color-warning-light-9); border-radius: 8px; padding: 8px 12px; margin-bottom: 16px; }
.info-notes p { margin: 4px 0; line-height: 1.6; overflow-wrap: anywhere; }
.info-section { border: 1px solid var(--el-border-color-lighter); border-radius: 10px; margin-bottom: 14px; overflow: hidden; }
.info-section h3 { margin: 0; padding: 11px 16px; font-size: 14px; background: var(--el-fill-color-light); }
.info-section dl { margin: 0; }
.info-field { display: grid; grid-template-columns: 170px minmax(0, 1fr); padding: 10px 16px; gap: 12px; }
.info-field + .info-field { border-top: 1px solid var(--el-border-color-lighter); }
.info-field dt { color: var(--el-text-color-secondary); line-height: 1.65; }
.info-field dd { display: flex; align-items: flex-start; flex-wrap: wrap; gap: 8px 16px; margin: 0; min-width: 0; }
.value { white-space: pre-wrap; overflow-wrap: anywhere; line-height: 1.65; user-select: text; }
.info-field .el-button { flex-shrink: 0; padding-top: 3px; }
.info-error { margin-bottom: 12px; flex-shrink: 0; }
.info-tip { margin-right: 16px; }
@media (max-width: 650px) { .info-field { grid-template-columns: 1fr; gap: 4px; } .info-tip { display: none; } }
</style>
