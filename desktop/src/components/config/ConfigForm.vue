<script setup lang="ts">
// 用表单编辑 ferry.yaml：左边是项目、构建、服务器、应用、数据备份，右边列出选中那一项的全部配置。
// 改动直接写回文字（v-model:text），和「YAML」页是同一份内容。
import { computed, markRaw, nextTick, provide, reactive, ref, watch } from 'vue'
import { ElMessageBox } from 'element-plus'
import { ArrowDown, ArrowRight, ArrowUp, CopyDocument, Delete, EditPen, Plus } from '@element-plus/icons-vue'
import { isMap, isScalar, isSeq, type Document } from 'yaml'
import { schema, type SchemaNode } from '../../editor/schema'
import { addEntry, append, keysAt, move, nodeAt, parse, remove, renameKey, scalarAt, serialize, type Path } from '../../editor/configDoc'
import ConfigFields from './ConfigFields.vue'
import { formCtx } from './ctx'

const props = defineProps<{
  text: string
  /** ferry 校验的错误（没有问题时为空） */
  errors: string
}>()
const emit = defineEmits<{ 'update:text': [text: string] }>()

const doc = computed(() => markRaw(parse(props.text)))
const syntaxError = computed(() => {
  const e = doc.value.errors[0]
  if (!e) return ''
  return e.linePos ? `第 ${e.linePos[0].line} 行附近` : '在「YAML」页能看到标红的地方'
})

function edit(fn: (d: Document) => void) {
  const d = parse(props.text)
  if (d.errors.length) return
  fn(d)
  const out = serialize(props.text, d)
  if (out !== props.text) emit('update:text', out)
}

const names = computed(() => {
  const d = doc.value
  const apps = nodeAt(d, ['apps'])
  return {
    builds: keysAt(d, ['builds']),
    targets: keysAt(d, ['targets']),
    apps: isSeq(apps) ? apps.items.map((a, i) => (isMap(a) ? String(a.get('name') ?? '') : '') || `第 ${i + 1} 个`) : [],
  }
})

const onlySet = ref(false)
provide(formCtx, { doc, edit, names, onlySet })

// ---------------------------------------------------------------- 左边的列表

type Section = 'project' | 'builds' | 'targets' | 'apps' | 'backups'
interface Sel {
  s: Section
  /** builds、targets 是名字，apps、backups 是序号 */
  key?: string | number
}

const sel = ref<Sel>({ s: 'project' })

// 应用通常最多，放在最后：前面几组不会被挤到列表下面看不见
const sections: { s: Exclude<Section, 'project'>; label: string; add: string; unit: string }[] = [
  { s: 'targets', label: '服务器', add: '新增服务器', unit: '台' },
  { s: 'builds', label: '构建', add: '新增构建', unit: '条' },
  { s: 'backups', label: '数据备份', add: '新增备份', unit: '项' },
  { s: 'apps', label: '应用', add: '新增应用', unit: '个' },
]

// 左边收起的组
const folded = reactive(new Set<Section>())
function toggleGroup(s: Section) {
  if (folded.has(s)) folded.delete(s)
  else folded.add(s)
}

function openSection(s: Exclude<Section, 'project'>) {
  folded.delete(s)
  const first = itemsOf(s)[0]
  if (first) sel.value = { s, key: first.key }
}

function itemsOf(s: Section): { key: string | number; title: string; sub: string }[] {
  const d = doc.value
  if (s === 'builds') return keysAt(d, ['builds']).map((k) => ({ key: k, title: k, sub: String(scalarAt(d, ['builds', k, 'command']) ?? '') }))
  if (s === 'targets')
    return keysAt(d, ['targets']).map((k) => {
      const title = scalarAt(d, ['targets', k, 'title'])
      return { key: k, title: title ? `${title}（${k}）` : k, sub: String(scalarAt(d, ['targets', k, 'host']) ?? '') }
    })
  if (s === 'apps') return names.value.apps.map((n, i) => ({ key: i, title: n, sub: String(scalarAt(d, ['apps', i, 'type']) ?? '') }))
  if (s === 'backups') {
    const n = nodeAt(d, ['backups'])
    if (!isSeq(n)) return []
    return n.items.map((_, i) => {
      const kind = ['mysql', 'postgres', 'paths', 'command'].find((k) => nodeAt(d, ['backups', i, k]) !== undefined)
      return { key: i, title: String(scalarAt(d, ['backups', i, 'name']) ?? '') || `第 ${i + 1} 个`, sub: kind ? kindLabel[kind] : '' }
    })
  }
  return []
}

const kindLabel: Record<string, string> = { mysql: 'MySQL', postgres: 'PostgreSQL', paths: '目录或文件', command: '命令' }

// 选中的那项在文档里的路径和结构说明
const current = computed<{ path: Path; node: SchemaNode; title: string; appType?: string } | null>(() => {
  const { s, key } = sel.value
  const f = schema.fields!
  if (s === 'project') return { path: [], node: { ...schema, doc: '整个项目共用的设置。服务器、构建、数据备份、应用各有自己的设置：点下面的卡片或左边列表打开' }, title: '项目' }
  const path: Path = [s, key!]
  const n = nodeAt(doc.value, path)
  if (n === undefined) return null
  if (s === 'builds') return { path, node: f.builds.map!, title: `构建 ${key}` }
  if (s === 'targets') return { path, node: f.targets.map!, title: `服务器 ${key}` }
  if (s === 'apps') return { path, node: f.apps.item!, title: `应用 ${names.value.apps[key as number]}`, appType: String(scalarAt(doc.value, [...path, 'type']) ?? '') || undefined }
  return { path, node: f.backups.item!, title: `数据备份 ${itemsOf('backups')[key as number]?.title ?? ''}` }
})

// 删除、改名后选中的那项没有了：回到项目
watch(current, (c) => {
  if (!c && !syntaxError.value) sel.value = { s: 'project' }
})

// ---------------------------------------------------------------- 哪些地方有错误（左边标红点）

const errorLines = computed(() =>
  props.errors
    .split('\n')
    .map((l) => l.trim())
    .filter((l) => l && !/ferry\.ya?ml:?$/i.test(l)),
)

function errorsOf(s: Section, key?: string | number): string[] {
  return errorLines.value.filter((l) => {
    if (s === 'project') return !/^(targets|builds|apps|backups)[.[]/.test(l)
    if (s === 'builds' || s === 'targets') return l.startsWith(`${s}.${key}：`) || l.startsWith(`${s}.${key}.`) || l.startsWith(`${s}.${key}（`)
    const i = key as number
    const name = s === 'apps' ? names.value.apps[i] : String(scalarAt(doc.value, ['backups', i, 'name']) ?? '')
    return l.startsWith(`${s}[${i}]`) || (!!name && (l.startsWith(`${s}.${name}：`) || l.startsWith(`${s}.${name}.`) || l.startsWith(`${s}.${name}（`)))
  })
}

const selErrors = computed(() => errorsOf(sel.value.s, sel.value.key))
// 服务器里报的跟应用有关的问题（如没有运行用户）也算在服务器上，这里只显示一次
const otherErrors = computed(() => errorLines.value.length - selErrors.value.length)

// ---------------------------------------------------------------- 新增、删除、改名、移动

interface NewField {
  key: string
  label: string
  placeholder?: string
  values?: string[]
  check?: (v: string) => string
}
const adding = reactive<{ open: boolean; s: Exclude<Section, 'project'>; fields: NewField[]; form: Record<string, string>; err: string }>({
  open: false,
  s: 'targets',
  fields: [],
  form: {},
  err: '',
})

const nameOk = (re: RegExp, what: string, taken: () => string[]) => (v: string) => {
  if (!re.test(v)) return `${what}只能用字母、数字、. _ -，字母或数字开头`
  if (taken().some((x) => x.toLowerCase() === v.toLowerCase())) return `已经有叫 ${v} 的了`
  return ''
}

const newFields: Record<Exclude<Section, 'project'>, NewField[]> = {
  builds: [
    { key: 'name', label: '名字', placeholder: '如 backend', check: nameOk(/^[A-Za-z0-9][A-Za-z0-9._-]*$/, '名字', () => names.value.builds) },
    { key: 'command', label: '构建命令', placeholder: '如 mvn -q package -DskipTests' },
  ],
  targets: [
    { key: 'name', label: '名字', placeholder: '如 prod、test-1', check: nameOk(/^[A-Za-z0-9][A-Za-z0-9._-]*$/, '名字', () => names.value.targets) },
    { key: 'host', label: '服务器地址', placeholder: 'IP 或域名' },
  ],
  apps: [
    { key: 'name', label: '应用名', placeholder: '如 api、web', check: nameOk(/^[A-Za-z0-9][A-Za-z0-9._-]*$/, '应用名', () => names.value.apps) },
    { key: 'type', label: '类型', values: ['java', 'binary', 'static', 'python', 'node', 'dotnet'] },
    { key: 'path', label: '产物路径', placeholder: 'jar、程序文件或目录，相对配置文件所在目录' },
  ],
  backups: [
    {
      key: 'name',
      label: '名字',
      placeholder: '如 db、uploads',
      check: (v) => (/^[a-z0-9][a-z0-9-]{0,31}$/.test(v) ? (itemsOf('backups').some((b) => b.title === v) ? `已经有叫 ${v} 的了` : '') : '只能用小写字母、数字、横线'),
    },
    { key: 'kind', label: '备份什么', values: ['mysql', 'postgres', 'paths', 'command'] },
    { key: 'value', label: '库名 / 目录 / 命令', placeholder: 'MySQL、PostgreSQL 填库名；目录填服务器上的绝对路径；命令填输出备份内容的命令' },
  ],
}

function startAdd(s: Exclude<Section, 'project'>) {
  adding.s = s
  adding.fields = newFields[s]
  adding.form = Object.fromEntries(newFields[s].map((f) => [f.key, f.values?.[0] ?? '']))
  adding.err = ''
  adding.open = true
}

function confirmAdd() {
  const f = adding.form
  for (const nf of adding.fields) {
    const v = (f[nf.key] ?? '').trim()
    if (!v) return (adding.err = `要填${nf.label}`)
    const e = nf.check?.(v)
    if (e) return (adding.err = e)
  }
  const s = adding.s
  let next: Sel = { s }
  edit((d) => {
    if (s === 'builds') addEntry(d, ['builds'], f.name.trim(), { command: f.command.trim() })
    else if (s === 'targets') addEntry(d, ['targets'], f.name.trim(), { host: f.host.trim() })
    else if (s === 'apps') next.key = append(d, ['apps'], { name: f.name.trim(), type: f.type, artifact: { path: f.path.trim() } })
    else {
      const v = f.value.trim()
      const what = f.kind === 'mysql' || f.kind === 'postgres' ? { database: v } : f.kind === 'paths' ? [v] : v
      next.key = append(d, ['backups'], { name: f.name.trim(), [f.kind]: what })
    }
  })
  if (s === 'builds' || s === 'targets') next = { s, key: f.name.trim() }
  adding.open = false
  nextTick(() => (sel.value = next))
}

async function removeSel() {
  const c = current.value
  if (!c || sel.value.s === 'project') return
  try {
    await ElMessageBox.confirm(`删除${c.title}？保存之前还可以在「YAML」页或用「撤销全部修改」找回。`, '删除', { type: 'warning' })
  } catch {
    return
  }
  edit((d) => remove(d, c.path, 1))
  sel.value = { s: 'project' }
}

async function renameSel() {
  const { s, key } = sel.value
  if (s !== 'builds' && s !== 'targets') return
  const check = newFields[s][0].check!
  let to: string
  try {
    const r = (await ElMessageBox.prompt(
      s === 'targets' ? '改名后，本机设置、告警等按名字记录的东西要重新设置' : '引用了这条构建的应用会一起改',
      `改名：${key}`,
      { inputValue: String(key), inputValidator: (v) => v.trim() === key || check(v.trim()) || true },
    )) as { value: string }
    to = r.value.trim()
  } catch {
    return
  }
  if (to === key) return
  edit((d) => {
    renameKey(d, [s], String(key), to)
    if (s === 'builds') {
      // 应用里引用这条构建的一起改
      const apps = nodeAt(d, ['apps'])
      if (isSeq(apps)) apps.items.forEach((a, i) => {
        const b = nodeAt(d, ['apps', i, 'build'])
        if (isScalar(b) && b.value === key) b.value = to
      })
    }
  })
  sel.value = { s, key: to }
}

function moveSel(delta: number) {
  const { s, key } = sel.value
  if (s !== 'apps' && s !== 'backups') return
  const i = key as number
  const n = nodeAt(doc.value, [s])
  if (!isSeq(n) || i + delta < 0 || i + delta >= n.items.length) return
  edit((d) => move(d, [s], i, i + delta))
  sel.value = { s, key: i + delta }
}

function copySel() {
  const { s, key } = sel.value
  if (s !== 'apps') return
  let next = -1
  edit((d) => {
    const n = nodeAt(d, ['apps', key as number])
    if (!isMap(n)) return
    const copy = n.clone()
    const base = String(n.get('name') ?? 'app')
    let name = `${base}-copy`
    for (let k = 2; names.value.apps.some((a) => a.toLowerCase() === name.toLowerCase()); k++) name = `${base}-copy${k}`
    ;(copy as typeof n).set('name', name)
    // 服务名不能重复：复制出来的不带
    const svc = (copy as typeof n).get('service', true)
    if (isMap(svc)) svc.delete('unit')
    next = append(d, ['apps'], copy)
  })
  if (next >= 0) nextTick(() => (sel.value = { s: 'apps', key: next }))
}

const listLen = (s: Section) => itemsOf(s).length

// 项目页不列左边单独列出的几项；kind 只有「新建连接」的文件才用，没写就不列
const projectExclude = computed(() => ['builds', 'targets', 'apps', 'backups', ...(scalarAt(doc.value, ['kind']) == null ? ['kind'] : [])])

// ---------------------------------------------------------------- 从项目页点进来：定位到字段

/** 选中 path 所在的服务器 / 应用……，滚动到那个字段并闪一下。 */
function focusPath(path: string[]) {
  const [s, key] = path
  if (s === 'targets' || s === 'builds') sel.value = { s, key }
  else if (s === 'apps') {
    const i = names.value.apps.findIndex((a) => a === key)
    if (i >= 0) sel.value = { s, key: i }
  }
  nextTick(() => {
    const at = [s, s === 'apps' ? sel.value.key : key, ...path.slice(2)].join('.')
    const el = panel.value?.querySelector(`[data-path="${CSS.escape(at)}"]`)
    if (!el) return
    el.scrollIntoView({ block: 'center' })
    el.classList.add('flash')
    setTimeout(() => el.classList.remove('flash'), 1700)
    el.querySelector<HTMLInputElement>('input')?.focus()
  })
}

const panel = ref<HTMLElement>()
defineExpose({ focusPath })

function isSel(s: Section, key?: string | number) {
  return sel.value.s === s && sel.value.key === key
}
</script>

<template>
  <div class="cfg-form">
    <el-alert v-if="syntaxError" type="error" :closable="false" show-icon class="syntax" :title="`YAML 写法有错（${syntaxError}），表单用不了，请先在「YAML」页改好`" />
    <template v-else>
      <nav class="nav">
        <div class="nav-item" :class="{ on: isSel('project') }" @click="sel = { s: 'project' }">
          <span class="nav-title">项目</span>
          <span v-if="errorsOf('project').length" class="dot" />
          <span class="nav-sub mono">{{ scalarAt(doc, ['project']) }}</span>
        </div>
        <template v-for="sec in sections" :key="sec.s">
          <div class="nav-group" :title="folded.has(sec.s) ? '展开' : '收起'" @click="toggleGroup(sec.s)">
            <el-icon class="fold"><ArrowRight v-if="folded.has(sec.s)" /><ArrowDown v-else /></el-icon>
            <span class="group-label">{{ sec.label }}</span>
            <span class="muted small">{{ listLen(sec.s) }}</span>
            <span v-if="!listLen(sec.s) && schema.fields![sec.s].required" class="ev-error small">至少要一个</span>
            <span class="spacer" />
            <el-button size="small" text type="primary" :icon="Plus" :title="sec.add" @click.stop="startAdd(sec.s)">新增</el-button>
          </div>
          <template v-if="!folded.has(sec.s)">
            <div v-for="it in itemsOf(sec.s)" :key="it.key" class="nav-item" :class="{ on: isSel(sec.s, it.key) }" @click="sel = { s: sec.s, key: it.key }">
              <span class="nav-title">{{ it.title }}</span>
              <span v-if="errorsOf(sec.s, it.key).length" class="dot" />
              <span class="nav-sub mono">{{ it.sub }}</span>
            </div>
            <div v-if="!listLen(sec.s)" class="nav-empty" @click="startAdd(sec.s)">还没有{{ sec.label }}，点这里{{ sec.add }}</div>
          </template>
        </template>
      </nav>

      <section ref="panel" class="panel">
        <template v-if="current">
          <div class="panel-head">
            <h3>{{ current.title }}</h3>
            <span class="spacer" />
            <el-radio-group v-model="onlySet" size="small">
              <el-radio-button :value="false">全部配置</el-radio-button>
              <el-radio-button :value="true">只看写了的</el-radio-button>
            </el-radio-group>
            <template v-if="sel.s !== 'project'">
              <el-button v-if="sel.s === 'builds' || sel.s === 'targets'" size="small" :icon="EditPen" @click="renameSel">改名</el-button>
              <template v-if="sel.s === 'apps' || sel.s === 'backups'">
                <el-button size="small" :icon="ArrowUp" :disabled="sel.key === 0" title="上移" aria-label="上移" @click="moveSel(-1)" />
                <el-button size="small" :icon="ArrowDown" :disabled="sel.key === listLen(sel.s) - 1" title="下移" aria-label="下移" @click="moveSel(1)" />
              </template>
              <el-button v-if="sel.s === 'apps'" size="small" :icon="CopyDocument" @click="copySel">复制</el-button>
              <el-button size="small" type="danger" plain :icon="Delete" @click="removeSel">删除</el-button>
            </template>
          </div>
          <p class="muted desc">{{ current.node.doc }}</p>
          <div v-if="selErrors.length" class="errs">
            <div v-for="(e, i) in selErrors" :key="i" class="ev-error">{{ e }}</div>
          </div>
          <div v-if="otherErrors > 0" class="muted small other-errs">其他地方还有 {{ otherErrors }} 个问题（左边标了红点）</div>
          <!-- 项目页：服务器、构建、数据备份、应用在左边单独列，这里也列出来，没写的也看得到 -->
          <div v-if="sel.s === 'project'" class="overview">
            <div v-for="sec in sections" :key="sec.s" class="ov-card">
              <div class="ov-head">
                <span class="ov-label">{{ sec.label }}</span>
                <span class="key mono">{{ sec.s }}</span>
                <span :class="listLen(sec.s) ? 'muted' : schema.fields![sec.s].required ? 'ev-error' : 'muted'" class="small">
                  {{ listLen(sec.s) ? `${listLen(sec.s)} ${sec.unit}` : schema.fields![sec.s].required ? '还没有，至少要一个' : '还没有' }}
                </span>
                <span class="spacer" />
                <el-button v-if="listLen(sec.s)" size="small" @click="openSection(sec.s)">查看</el-button>
                <el-button size="small" type="primary" plain :icon="Plus" @click="startAdd(sec.s)">{{ sec.add }}</el-button>
              </div>
              <div class="muted small">{{ schema.fields![sec.s].doc }}</div>
            </div>
          </div>
          <ConfigFields
            :key="JSON.stringify(current.path)"
            :path="current.path"
            :node="current.node"
            :app-type="current.appType"
            :keep="current.path.length"
            :exclude="sel.s === 'project' ? projectExclude : undefined"
          />
        </template>
      </section>
    </template>

    <el-dialog v-model="adding.open" :title="sections.find((x) => x.s === adding.s)?.add" width="480px" append-to-body>
      <el-form label-width="120px" @submit.prevent="confirmAdd">
        <el-form-item v-for="f in adding.fields" :key="f.key" :label="f.label">
          <el-select v-if="f.values" v-model="adding.form[f.key]">
            <el-option v-for="v in f.values" :key="v" :label="adding.s === 'backups' ? kindLabel[v] : v" :value="v" />
          </el-select>
          <el-input v-else v-model="adding.form[f.key]" :placeholder="f.placeholder" @keyup.enter="confirmAdd" />
        </el-form-item>
      </el-form>
      <p class="muted small">其他配置添加后在右边填写。</p>
      <template #footer>
        <span v-if="adding.err" class="ev-error add-err">{{ adding.err }}</span>
        <el-button @click="adding.open = false">取消</el-button>
        <el-button type="primary" @click="confirmAdd">添加</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.cfg-form {
  display: flex;
  height: 100%;
  min-height: 0;
}
.syntax {
  align-self: flex-start;
}
.nav {
  width: 210px;
  flex: none;
  overflow-y: auto;
  border-right: 1px solid var(--el-border-color-lighter);
  padding: 4px 6px 12px 0;
}
.nav-group {
  display: flex;
  align-items: center;
  gap: 6px;
  margin: 14px 0 2px;
  padding: 2px 0 2px 4px;
  cursor: pointer;
  user-select: none;
  border-top: 1px solid var(--el-border-color-lighter);
  padding-top: 8px;
}
.group-label {
  font-weight: 600;
  font-size: 13px;
}
.fold {
  color: var(--el-text-color-secondary);
}
.nav-empty {
  margin: 2px 0 2px 8px;
  padding: 6px 8px;
  border: 1px dashed var(--el-border-color);
  border-radius: 6px;
  font-size: 12px;
  color: var(--el-text-color-secondary);
  cursor: pointer;
}
.nav-empty:hover {
  color: var(--el-color-primary);
  border-color: var(--el-color-primary-light-5);
}
.overview {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(320px, 1fr));
  gap: 8px;
  margin: 0 0 12px;
}
.ov-card {
  border: 1px solid var(--el-border-color-lighter);
  border-radius: 8px;
  padding: 8px 10px;
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.ov-head {
  display: flex;
  align-items: center;
  gap: 6px;
}
.ov-label {
  font-weight: 600;
}
.key {
  font-size: 11.5px;
  color: var(--el-text-color-secondary);
}
.nav-item {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  column-gap: 6px;
  padding: 5px 8px;
  border-radius: 6px;
  cursor: pointer;
}
.nav-item:hover {
  background: var(--el-fill-color-light);
}
.nav-item.on {
  background: var(--el-color-primary-light-9);
  color: var(--el-color-primary);
}
.nav-title {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  max-width: 100%;
}
.nav-sub {
  width: 100%;
  font-size: 11.5px;
  color: var(--el-text-color-secondary);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.nav-sub:empty {
  display: none;
}
.dot {
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: var(--el-color-danger);
  flex: none;
}
.panel {
  flex: 1;
  min-width: 0;
  overflow-y: auto;
  padding: 0 8px 24px 16px;
}
.panel-head {
  display: flex;
  align-items: center;
  gap: 8px;
  position: sticky;
  top: 0;
  z-index: 2;
  background: var(--el-bg-color);
  padding: 4px 0 6px;
}
.panel-head h3 {
  margin: 0;
  font-size: 16px;
}
.panel-head .el-radio-group {
  margin-right: 8px;
}
.desc {
  margin: 0 0 8px;
  font-size: 12.5px;
}
.errs {
  margin: 0 0 8px;
  padding: 6px 10px;
  border-radius: 6px;
  background: var(--el-color-danger-light-9);
  font-size: 12.5px;
  line-height: 1.6;
}
.other-errs {
  margin: 0 0 8px;
}
.spacer {
  flex: 1;
}
.small {
  font-size: 12px;
}
.add-err {
  margin-right: 12px;
  font-size: 13px;
}
</style>
