<script setup lang="ts">
// 编程软件能用的 MCP 工具（设置 → 命令行与 MCP → MCP 工具列表）：每个工具的作用、参数、是否要确认。
// 内容取自 MCP 服务里注册的工具（ferry mcp-tools），和编程软件看到的一样。
import { computed, onMounted, ref, watch } from 'vue'
import { Search } from '@element-plus/icons-vue'
import { call } from '../ferry'

const props = defineProps<{ allowChanges: boolean }>()
const emit = defineEmits<{ close: [] }>()
const visible = ref(true)

interface Param {
  name: string
  type: string
  required: boolean
  description?: string
  depth: number
}

interface Tool {
  name: string
  title: string
  description: string
  change: boolean
  server: boolean
  params: Param[]
}

const tools = ref<Tool[]>([])
const loading = ref(true)
const error = ref('')

onMounted(async () => {
  try {
    tools.value = (await call<{ tools: Tool[] }>(['mcp-tools'])).tools
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
})

// ---------------------------------------------------------------- 筛选

const kind = ref<'' | 'read' | 'change'>('')
const keyword = ref('')

const readCount = computed(() => tools.value.filter((t) => !t.change).length)
const changeCount = computed(() => tools.value.filter((t) => t.change).length)

function matches(t: Tool, q: string) {
  if (!q) return true
  const hay = [t.name, t.title, t.description, ...t.params.flatMap((p) => [p.name, p.description ?? ''])]
  return hay.some((s) => s.toLowerCase().includes(q))
}

// 查看的在前，改动的在后（各自按注册的顺序）
const shown = computed(() => {
  const q = keyword.value.trim().toLowerCase()
  const list = tools.value.filter((t) => (kind.value === '' || (kind.value === 'change') === t.change) && matches(t, q))
  return [...list.filter((t) => !t.change), ...list.filter((t) => t.change)]
})

// ---------------------------------------------------------------- 显示

const BASE_TYPES: Record<string, string> = {
  string: '文字',
  integer: '整数',
  number: '数字',
  boolean: '是 / 否',
  object: '对象',
  map: '键值对',
}

function typeLabel(t: string): string {
  if (t.endsWith('[]')) return typeLabel(t.slice(0, -2)) + '列表'
  return BASE_TYPES[t] ?? (t || '-')
}

// 嵌套的参数只显示最后一段，前面用缩进表示属于谁
const shortName = (p: Param) => (p.depth ? p.name.slice(p.name.lastIndexOf('.') + 1) : p.name)

const listEl = ref<HTMLElement>()
const active = ref('')
// 左侧高亮的工具：还没滚动时是第一个
const current = computed(() => active.value || shown.value[0]?.name || '')

// 换了筛选回到最上面
watch([kind, keyword], () => {
  active.value = ''
  if (listEl.value) listEl.value.scrollTop = 0
})

function jump(name: string) {
  active.value = name
  listEl.value?.querySelector(`[data-tool="${name}"]`)?.scrollIntoView({ behavior: 'smooth', block: 'start' })
}

// 滚动时左侧跟着高亮当前看到的工具
function onScroll() {
  const box = listEl.value
  if (!box) return
  const top = box.getBoundingClientRect().top
  let cur = ''
  for (const el of box.querySelectorAll<HTMLElement>('[data-tool]')) {
    if (el.getBoundingClientRect().top - top > 40) break
    cur = el.dataset.tool ?? ''
  }
  active.value = cur
}
</script>

<template>
  <el-dialog v-model="visible" title="MCP 工具列表" width="90%" top="4vh" append-to-body @closed="emit('close')">
    <div class="tools-workspace">
    <div class="bar">
      <el-radio-group v-model="kind" size="small">
        <el-radio-button value="">全部 {{ tools.length }}</el-radio-button>
        <el-radio-button value="read">查看 {{ readCount }}</el-radio-button>
        <el-radio-button value="change">改动 {{ changeCount }}</el-radio-button>
      </el-radio-group>
      <el-input v-model="keyword" size="small" clearable :prefix-icon="Search" placeholder="搜索工具名、作用、参数…" class="keyword" />
    </div>
    <p class="muted small intro">
      编程软件（Claude Code、Cursor 等）接入 Ferry 后能用下面这些工具，AI 会按需要自己挑着用。
      「查看」类只读取信息，直接执行；「改动」类每次都会在 Ferry 里弹出确认，你点「同意执行」才做，生产服务器始终不允许。
      <span v-if="!props.allowChanges" class="warn">「允许编程软件请求改动」现在没打开，改动类工具会被拒绝。</span>
    </p>
    <el-alert v-if="error" type="error" :title="error" :closable="false" show-icon class="mb" />
    <div v-loading="loading" class="layout">
      <nav class="nav">
        <template v-for="(t, i) in shown" :key="t.name">
          <div v-if="i === 0 || shown[i - 1].change !== t.change" class="group">{{ t.change ? '改动（要你确认）' : '查看' }}</div>
          <a :class="['item', { on: current === t.name }]" @click="jump(t.name)">
            <span>{{ t.title }}</span>
            <span class="mono muted">{{ t.name }}</span>
          </a>
        </template>
      </nav>
      <div ref="listEl" class="list" @scroll.passive="onScroll">
        <div v-if="!loading && !shown.length && !error" class="muted empty">没有符合的工具</div>
        <section v-for="t in shown" :key="t.name" :data-tool="t.name" class="tool">
          <div class="head">
            <span class="title">{{ t.title }}</span>
            <span class="mono name">{{ t.name }}</span>
            <el-tag v-if="t.change" size="small" type="warning" disable-transitions>改动 · 要你确认</el-tag>
            <el-tag v-else size="small" type="success" disable-transitions>只读</el-tag>
            <el-tag size="small" type="info" disable-transitions>{{ t.server ? '连服务器' : '只用本机' }}</el-tag>
          </div>
          <p class="desc">{{ t.description }}</p>
          <table v-if="t.params.length" class="params">
            <thead>
              <tr>
                <th class="c-name">参数</th>
                <th class="c-type">类型</th>
                <th class="c-req">必填</th>
                <th>说明</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="p in t.params" :key="p.name">
                <td class="mono c-name" :style="{ paddingLeft: 8 + p.depth * 16 + 'px' }" :title="p.name">{{ shortName(p) }}</td>
                <td class="c-type">{{ typeLabel(p.type) }}</td>
                <td class="c-req">{{ p.required ? '必填' : '' }}</td>
                <td>{{ p.description || '-' }}</td>
              </tr>
            </tbody>
          </table>
          <div v-else class="muted small">不用参数</div>
        </section>
      </div>
    </div>
    </div>
  </el-dialog>
</template>

<style scoped>
.tools-workspace { display: flex; flex-direction: column; height: min(680px, calc(100dvh - 180px)); min-height: 240px; }
.tools-workspace > :not(.layout) { flex-shrink: 0; }
.bar {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
}
.bar .keyword {
  width: 260px;
}
.intro {
  margin: 8px 0 10px;
  line-height: 1.6;
}
.small {
  font-size: 12px;
}
.mb {
  margin-bottom: 8px;
}
.warn {
  color: var(--el-color-warning);
}
.layout {
  display: flex;
  gap: 12px;
  flex: 1;
  min-height: 0;
}
.nav {
  flex: none;
  width: 220px;
  overflow: auto;
  border-right: 1px solid var(--el-border-color-lighter);
  padding-right: 8px;
}
.group {
  margin: 10px 0 4px 8px;
  font-size: 12px;
  color: var(--el-text-color-secondary);
}
.group:first-child {
  margin-top: 0;
}
.item {
  display: flex;
  flex-direction: column;
  padding: 4px 8px;
  border-radius: 6px;
  cursor: pointer;
  line-height: 1.4;
}
.item .mono {
  font-size: 11px;
}
.item:hover {
  background: var(--el-fill-color-light);
}
.item.on {
  background: var(--el-color-primary-light-9);
  color: var(--el-color-primary);
}
.list {
  flex: 1;
  min-width: 0;
  overflow: auto;
  padding-right: 4px;
}
.empty {
  padding: 40px 0;
  text-align: center;
}
.tool {
  padding: 12px 14px;
  border: 1px solid var(--el-border-color-lighter);
  border-radius: 8px;
  margin-bottom: 10px;
}
.head {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
}
.title {
  font-weight: 600;
  font-size: 15px;
}
.name {
  font-size: 12px;
  color: var(--el-text-color-secondary);
}
.desc {
  margin: 8px 0 10px;
  line-height: 1.7;
  font-size: 13px;
}
.params {
  width: 100%;
  border-collapse: collapse;
  font-size: 12px;
  table-layout: fixed;
}
.params th,
.params td {
  border: 1px solid var(--el-border-color-lighter);
  padding: 4px 8px;
  text-align: left;
  vertical-align: top;
  line-height: 1.5;
  overflow-wrap: anywhere;
}
.params th {
  background: var(--el-fill-color-light);
  font-weight: 500;
  color: var(--el-text-color-secondary);
}
.c-name {
  width: 160px;
  word-break: break-all;
}
.c-type {
  width: 80px;
  white-space: nowrap;
}
.c-req {
  width: 44px;
  white-space: nowrap;
}
@media (max-width: 760px) {
  .nav {
    display: none;
  }
}
</style>
