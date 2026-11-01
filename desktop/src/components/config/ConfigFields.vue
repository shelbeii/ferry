<script setup lang="ts">
// 表单里一个对象的全部配置：写了的、没写的都列出来（带说明和默认值），嵌套的对象一段一段展开。
import { computed, inject, reactive, ref } from 'vue'
import { ElMessageBox } from 'element-plus'
import { ArrowDown, ArrowRight, ArrowUp, Delete, Plus } from '@element-plus/icons-vue'
import type { Document } from 'yaml'
import type { SchemaNode } from '../../editor/schema'
import {
  append,
  expandShort,
  has,
  hasAlias,
  keysAt,
  listAt,
  mapAt,
  move,
  nodeAt,
  remove,
  scalarAt,
  setList,
  setMap,
  setScalar,
  type Path,
} from '../../editor/configDoc'
import { isMap, isScalar, isSeq } from 'yaml'
import FieldInput, { type FieldValue } from './FieldInput.vue'
import { formCtx } from './ctx'

defineOptions({ name: 'ConfigFields' })

const props = defineProps<{
  path: Path
  node: SchemaNode
  /** 所在应用的类型：只显示这种应用能用的配置 */
  appType?: string
  /** 删除后清理空对象时不删到这一层以上（选中的服务器、应用本身） */
  keep: number
  /** 不在这里显示的字段（项目页里的 targets、apps……在左边单独列） */
  exclude?: string[]
}>()

const ctx = inject(formCtx)!
const doc = computed(() => ctx.doc.value)

const self = computed(() => nodeAt(doc.value, props.path))
// 只写了一个值的写法（hooks 里的一条命令、附带文件）
const shortScalar = computed(() => !!props.node.short && props.node.short !== '@build' && isScalar(self.value))
const readonly = computed(() => hasAlias(doc.value, props.path))

const fields = computed(() => Object.entries(props.node.fields ?? {}).filter(([k]) => !props.exclude?.includes(k)))

function fieldPath(k: string): Path {
  return [...props.path, k]
}

function isSet(k: string): boolean {
  if (shortScalar.value) return k === props.node.short
  return has(doc.value, fieldPath(k))
}

function valueOf(k: string, c: SchemaNode): FieldValue {
  if (shortScalar.value) return k === props.node.short ? (scalarAt(doc.value, props.path) as FieldValue) : undefined
  const p = fieldPath(k)
  if (c.kind === 'list') return listAt(doc.value, p)
  if (c.kind === 'map') return mapAt(doc.value, p)
  return scalarAt(doc.value, p) as FieldValue
}

function required(c: SchemaNode): boolean {
  return c.required === true || (Array.isArray(c.required) && !!props.appType && c.required.includes(props.appType))
}

function visible(k: string, c: SchemaNode): boolean {
  if (props.appType && c.appTypes && !c.appTypes.includes(props.appType) && !isSet(k)) return false
  if (ctx.onlySet.value && !isSet(k) && !required(c)) return false
  return true
}

const hidden = computed(() => (props.appType ? fields.value.filter(([k, c]) => c.appTypes && !c.appTypes.includes(props.appType!) && !isSet(k)).length : 0))

const labelOf = (k: string) => props.node.fields?.[k]?.label ?? k

/** 不能写的原因（写了别的、缺前提）；已经写了的照样能改、能删。 */
function blocked(k: string, c: SchemaNode): string {
  if (isSet(k)) {
    if (c.appTypes && props.appType && !c.appTypes.includes(props.appType)) return `${props.appType} 应用用不到这项，可以删掉`
    if (c.needs && !isSet(c.needs)) return `要先填「${labelOf(c.needs)}」才有用`
    if (c.unless && isSet(c.unless)) return `填了「${labelOf(c.unless)}」时用不到这项`
    return ''
  }
  if (c.needs && !isSet(c.needs)) return `要先填「${labelOf(c.needs)}」`
  if (c.unless && isSet(c.unless)) return `填了「${labelOf(c.unless)}」时用不到`
  for (const g of props.node.oneOf ?? []) {
    if (!g.includes(k)) continue
    const other = g.find((o) => o !== k && isSet(o))
    if (other) return `和「${labelOf(other)}」只能写一个`
  }
  return ''
}

const groupHint = computed(() => {
  if (!props.node.oneOfRequired) return ''
  const g = props.node.oneOf?.[0] ?? []
  return g.some((k) => isSet(k)) ? '' : `${g.map(labelOf).join('、')} 要写一个`
})

const unknown = computed(() => {
  const known = new Set(Object.keys(props.node.fields ?? {}))
  return shortScalar.value || !isMap(self.value) ? [] : keysAt(doc.value, props.path).filter((k) => !known.has(k) && k !== '<<')
})

function options(k: string): string[] | undefined {
  if (props.path[0] === 'targets' && props.path.length === 2 && k === 'apps') return ctx.names.value.apps
  if (props.path[0] === 'backups' && props.path.length === 2 && k === 'targets') return ctx.names.value.targets
  return undefined
}

// ---------------------------------------------------------------- 写回

/** 字段在文档里的路径；只写了一个值的对象先改成对象写法。 */
function writePath(d: Document, k: string): Path {
  if (props.node.short && props.node.short !== '@build' && isScalar(nodeAt(d, props.path))) {
    if (k === props.node.short) return props.path
    expandShort(d, props.path, props.node.short)
  }
  return fieldPath(k)
}

function commit(k: string, c: SchemaNode, v: FieldValue) {
  ctx.edit((d) => {
    const p = writePath(d, k)
    if (v === undefined) {
      // 只写了一个值的对象，清空它就是写空
      if (p === props.path) setScalar(d, p, '')
      else remove(d, p, props.keep)
    } else if (c.kind === 'list') setList(d, p, v as (string | number)[], props.keep, c.single)
    else if (c.kind === 'map') setMap(d, p, v as [string, string][], props.keep)
    else setScalar(d, p, v as string | number | boolean)
  })
}

function removeField(k: string) {
  ctx.edit((d) => remove(d, writePath(d, k), props.keep))
}

// ---------------------------------------------------------------- 展开的段落

const opened = reactive(new Set<string>())
const closed = reactive(new Set<string>())

function isOpen(k: string): boolean {
  return opened.has(k) || (isSet(k) && !closed.has(k))
}

function toggle(k: string) {
  if (isOpen(k)) {
    opened.delete(k)
    closed.add(k)
  } else {
    closed.delete(k)
    opened.add(k)
  }
}

// ---------------------------------------------------------------- 对象列表（hooks、附带文件）

function itemCount(k: string): number {
  const n = nodeAt(doc.value, fieldPath(k))
  return isSeq(n) ? n.items.length : 0
}

async function addItem(k: string, c: SchemaNode) {
  const short = c.item?.short
  let value: unknown = {}
  if (short) {
    const f = c.item!.fields![short]
    try {
      const r = (await ElMessageBox.prompt(f.doc, `添加：${f.label}`, { inputValidator: (s) => !!s.trim() || `要填${f.label}` })) as { value: string }
      value = r.value.trim()
    } catch {
      return
    }
  }
  ctx.edit((d) => append(d, writePath(d, k), value))
  opened.add(k)
}

function moveItem(k: string, i: number, to: number) {
  ctx.edit((d) => move(d, fieldPath(k), i, to))
}

function removeItem(k: string, i: number) {
  ctx.edit((d) => remove(d, [...fieldPath(k), i], props.keep))
}

// ---------------------------------------------------------------- 应用的构建：不构建 / 引用 builds / 直接写

const buildMode = ref<'' | 'ref' | 'inline'>('')
const buildState = computed(() => {
  const n = nodeAt(doc.value, fieldPath('build'))
  if (isScalar(n)) return 'ref'
  if (isMap(n)) return 'inline'
  return buildMode.value || 'none'
})

function setBuildMode(m: string | number | boolean | undefined) {
  buildMode.value = m === 'ref' || m === 'inline' ? m : ''
  const names = ctx.names.value.builds
  ctx.edit((d) => {
    const p = fieldPath('build')
    remove(d, p, props.keep)
    if (m === 'ref' && names.length) setScalar(d, p, names[0])
  })
  if (m === 'inline') opened.add('build')
}

function setBuildRef(v: string) {
  ctx.edit((d) => setScalar(d, fieldPath('build'), v))
}

const buildRef = computed(() => (buildState.value === 'ref' ? String(scalarAt(doc.value, fieldPath('build')) ?? '') : ''))

const kindText: Record<string, string> = { list: '列表', map: '名字 → 值', dur: '时长', num: '数字' }
</script>

<template>
  <div class="fields">
    <el-alert v-if="readonly" type="warning" :closable="false" show-icon class="note" title="这里用了 YAML 的锚点或引用（& * <<），表单里只能看，请到「YAML」页修改" />
    <div v-if="groupHint" class="note ev-warn">{{ groupHint }}</div>

    <template v-for="[k, c] in fields" :key="k">
      <template v-if="visible(k, c)">
        <!-- 应用的构建 -->
        <div v-if="c.short === '@build'" class="sect" :class="{ set: isSet(k) }" :data-path="fieldPath(k).join('.')">
          <div class="sect-head static">
            <span class="label">{{ c.label }}</span>
            <span class="key mono">{{ k }}</span>
          </div>
          <div class="doc muted">{{ c.doc }}</div>
          <div class="build">
            <el-radio-group :model-value="buildState" size="small" :disabled="readonly" @update:model-value="setBuildMode">
              <el-radio-button value="none">不构建</el-radio-button>
              <el-radio-button value="ref">用「构建」里的</el-radio-button>
              <el-radio-button value="inline">直接写命令</el-radio-button>
            </el-radio-group>
            <template v-if="buildState === 'ref'">
              <el-select v-if="ctx.names.value.builds.length" :model-value="buildRef" size="small" class="build-ref" :disabled="readonly" @update:model-value="setBuildRef">
                <el-option v-for="b in ctx.names.value.builds" :key="b" :label="b" :value="b" />
              </el-select>
              <span v-else class="muted small">还没有构建，先在左边「构建」里新增一条</span>
            </template>
          </div>
          <div v-if="buildState === 'inline'" class="sect-body">
            <ConfigFields :path="fieldPath(k)" :node="c" :keep="keep" />
          </div>
        </div>

        <!-- 嵌套的对象 -->
        <div v-else-if="c.kind === 'obj'" class="sect" :class="{ set: isSet(k) }" :data-path="fieldPath(k).join('.')">
          <div class="sect-head" @click="toggle(k)">
            <el-icon class="arrow"><ArrowDown v-if="isOpen(k)" /><ArrowRight v-else /></el-icon>
            <span class="label">{{ c.label }}</span>
            <span class="key mono">{{ k }}</span>
            <el-tag v-if="required(c)" size="small" type="danger" effect="plain">必填</el-tag>
            <span v-if="!isSet(k)" class="muted small">没写</span>
            <span v-if="blocked(k, c)" class="ev-warn small">{{ blocked(k, c) }}</span>
            <span class="spacer" />
            <el-button v-if="isSet(k) && !readonly" size="small" text :icon="Delete" @click.stop="removeField(k)">删除整段</el-button>
          </div>
          <div class="doc muted">{{ c.doc }}</div>
          <div v-if="isOpen(k)" class="sect-body">
            <ConfigFields :path="fieldPath(k)" :node="c" :app-type="appType" :keep="keep" />
          </div>
        </div>

        <!-- 对象列表（hooks 的命令、附带文件） -->
        <div v-else-if="c.kind === 'objList' && c.item" class="sect" :class="{ set: isSet(k) }" :data-path="fieldPath(k).join('.')">
          <div class="sect-head" @click="toggle(k)">
            <el-icon class="arrow"><ArrowDown v-if="isOpen(k)" /><ArrowRight v-else /></el-icon>
            <span class="label">{{ c.label }}</span>
            <span class="key mono">{{ k }}</span>
            <span class="muted small">{{ itemCount(k) ? `${itemCount(k)} 项` : '没写' }}</span>
            <span v-if="blocked(k, c)" class="ev-warn small">{{ blocked(k, c) }}</span>
          </div>
          <div class="doc muted">{{ c.doc }}</div>
          <div v-if="isOpen(k)" class="sect-body">
            <div v-for="i in itemCount(k)" :key="i" class="card">
              <div class="card-head">
                <span class="muted small">第 {{ i }} 项</span>
                <span class="spacer" />
                <template v-if="!readonly">
                  <el-button size="small" text :icon="ArrowUp" :disabled="i === 1" aria-label="上移" @click="moveItem(k, i - 1, i - 2)" />
                  <el-button size="small" text :icon="ArrowDown" :disabled="i === itemCount(k)" aria-label="下移" @click="moveItem(k, i - 1, i)" />
                  <el-button size="small" text :icon="Delete" @click="removeItem(k, i - 1)">删除</el-button>
                </template>
              </div>
              <ConfigFields :path="[...fieldPath(k), i - 1]" :node="c.item" :app-type="appType" :keep="keep" />
            </div>
            <el-button v-if="!readonly" size="small" :icon="Plus" @click="addItem(k, c)">添加{{ c.item.label }}</el-button>
          </div>
        </div>

        <!-- 一个值、列表、键值对 -->
        <div v-else class="row" :class="{ set: isSet(k) }" :data-path="fieldPath(k).join('.')">
          <div class="row-head">
            <span class="label">{{ c.label }}</span>
            <span class="key mono">{{ k }}</span>
            <el-tag v-if="required(c)" size="small" type="danger" effect="plain">必填</el-tag>
            <span v-if="kindText[c.kind ?? '']" class="muted small">{{ kindText[c.kind ?? ''] }}</span>
          </div>
          <div class="row-body">
            <div class="row-ctl">
              <FieldInput
                :node="c"
                :value="valueOf(k, c)"
                :options="options(k)"
                :disabled="readonly || (!!blocked(k, c) && !isSet(k))"
                @commit="(v) => commit(k, c, v)"
              />
              <el-button v-if="isSet(k) && !readonly && !(shortScalar && k === node.short)" size="small" text class="del" @click="removeField(k)">删除</el-button>
            </div>
            <div v-if="blocked(k, c)" class="ev-warn small">{{ blocked(k, c) }}</div>
            <div v-if="c.doc" class="doc muted">{{ c.doc }}</div>
          </div>
        </div>
      </template>
    </template>

    <div v-if="unknown.length" class="note ev-warn">配置里还有不认识的字段：{{ unknown.join('、') }}（到「YAML」页查看）</div>
    <div v-if="hidden" class="muted small note">还有 {{ hidden }} 项是其他类型的应用才用的，没有列出</div>
  </div>
</template>

<style scoped>
.fields {
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.row {
  display: grid;
  grid-template-columns: 190px 1fr;
  gap: 4px 14px;
  padding: 8px 10px;
  border-radius: 6px;
}
.row:hover {
  background: var(--el-fill-color-light);
}
.row-head {
  display: flex;
  flex-wrap: wrap;
  align-items: baseline;
  gap: 2px 6px;
  padding-top: 6px;
}
.row:not(.set) .label {
  color: var(--el-text-color-regular);
  font-weight: normal;
}
.label {
  font-weight: 600;
}
.key {
  font-size: 11.5px;
  color: var(--el-text-color-secondary);
}
.row-body {
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 3px;
}
.row-ctl {
  display: flex;
  align-items: flex-start;
  gap: 6px;
}
.row-ctl > .del {
  margin-top: 6px;
}
.del {
  color: var(--el-text-color-secondary);
}
.doc {
  font-size: 12px;
  line-height: 1.5;
}
.sect {
  margin: 4px 0;
  border: 1px solid var(--el-border-color-lighter);
  border-radius: 8px;
  padding: 6px 10px;
}
.sect.set {
  border-color: var(--el-border-color);
}
.sect > .doc {
  padding-left: 20px;
}
.sect-head {
  display: flex;
  align-items: center;
  gap: 6px;
  cursor: pointer;
  user-select: none;
  min-height: 26px;
}
.sect-head.static {
  cursor: default;
}
.arrow {
  color: var(--el-text-color-secondary);
}
.sect-body {
  margin-top: 6px;
  padding-left: 10px;
  border-left: 2px solid var(--el-border-color-lighter);
}
.build {
  display: flex;
  align-items: center;
  gap: 10px;
  margin: 6px 0 2px;
}
.build-ref {
  width: 200px;
}
.card {
  border: 1px dashed var(--el-border-color);
  border-radius: 6px;
  padding: 4px 6px;
  margin-bottom: 6px;
}
.card-head {
  display: flex;
  align-items: center;
  padding: 0 4px;
}
.spacer {
  flex: 1;
}
.small {
  font-size: 12px;
}
.note {
  margin: 6px 0;
  font-size: 12.5px;
}
/* 从项目页点进来的字段闪一下 */
.flash {
  animation: flash 1.6s ease-out;
}
@keyframes flash {
  0%,
  40% {
    background: var(--el-color-primary-light-8);
  }
  100% {
    background: transparent;
  }
}
</style>
