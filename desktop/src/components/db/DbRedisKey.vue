<script setup lang="ts">
// Redis 的一个键：类型、过期时间、大小；字符串直接改，哈希、列表、集合、有序集合、流一行一项，加、改、删。
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Refresh, Delete, EditPen, Plus, CopyDocument, Timer, Search } from '@element-plus/icons-vue'
import {
  dbCall, editable, humanSize, redisTypes, rstr, ttlText, type DbConn, type RedisEdit, type RedisItem, type RedisKey, type RedisVal, type RedisValue,
} from '../../db'

const props = defineProps<{ conn: DbConn; db: string; rkey: RedisKey }>()
const emit = defineEmits<{ deleted: []; renamed: [key: RedisKey] }>()

const ro = computed(() => !!props.conn.readOnly)
const v = ref<RedisValue | null>(null)
const items = ref<RedisItem[]>([])
const loading = ref(false)
const error = ref('')
const match = ref('')
const desc = ref(false)
const PAGE = 200

// 字符串
const text = ref('')
const orig = ref('')
const view = ref<'text' | 'json'>('text')
const dirty = computed(() => v.value?.type === 'string' && text.value !== orig.value)

async function load(more = false) {
  loading.value = true
  error.value = ''
  try {
    const cur = v.value
    const r = await dbCall<RedisValue>(props.conn, 'redis-get', props.db, {
      get: {
        key: rstr(props.rkey), limit: PAGE, match: match.value ? globOf(match.value) : '', desc: desc.value,
        offset: more ? items.value.length : 0, cursor: more ? cur?.cursor ?? '' : '',
      },
    })
    v.value = r
    items.value = more ? [...items.value, ...(r.items ?? [])] : r.items ?? []
    if (!more && r.type === 'string') {
      text.value = orig.value = r.value?.t ?? ''
      view.value = 'text'
      // 存的是一行的 JSON（常见）：直接格式化着看，保存时压回一行
      if (editable(r.value) && !text.value.includes('\n') && text.value.length < 200_000 && pretty(text.value) !== null) setView('json')
    }
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}
onMounted(() => void load())

function globOf(s: string) {
  return /[*?[]/.test(s) ? s : `*${s}*`
}

async function edit(e: RedisEdit, ok = '已保存'): Promise<boolean> {
  try {
    await dbCall(props.conn, 'redis-edit', props.db, { edit: { key: rstr(props.rkey), ...e } })
    ElMessage.success({ message: ok, duration: 1500 })
    return true
  } catch (err) {
    void ElMessageBox.alert((err as Error).message, '没有成功', { type: 'error' })
    return false
  }
}

// ---------------------------------------------------------------- 字符串
function pretty(s: string): string | null {
  const t = s.trim()
  if (!t || !'[{'.includes(t[0])) return null
  try {
    return JSON.stringify(JSON.parse(t), null, 2)
  } catch {
    return null
  }
}
const jsonOK = computed(() => pretty(text.value) !== null)
function setView(m: 'text' | 'json') {
  if (m === 'json') {
    const p = pretty(text.value)
    if (p === null) return ElMessage.warning('不是 JSON')
    // 原来是一行的 JSON：格式化只是为了看，存的时候再压回一行
    text.value = p
  } else if (view.value === 'json' && !orig.value.includes('\n')) {
    try { text.value = JSON.stringify(JSON.parse(text.value)) } catch { /* 改坏了就原样 */ }
  }
  view.value = m
}
const dirtyJSON = computed(() => {
  if (view.value !== 'json') return dirty.value
  try {
    const now = orig.value.includes('\n') ? text.value : JSON.stringify(JSON.parse(text.value))
    return now !== orig.value
  } catch {
    return true
  }
})
async function saveString() {
  let val = text.value
  if (view.value === 'json' && !orig.value.includes('\n')) {
    try {
      val = JSON.stringify(JSON.parse(val))
    } catch {
      return ElMessage.warning('JSON 格式不对')
    }
  }
  if (await edit({ op: 'set', value: val })) {
    orig.value = val
    if (view.value === 'text') text.value = val
    void load()
  }
}

// ---------------------------------------------------------------- 键本身
async function rename() {
  let name: string
  try {
    const r = await ElMessageBox.prompt('新的键名', '重命名', { inputValue: props.rkey.b64 ? '' : props.rkey.name, confirmButtonText: '改名', cancelButtonText: '取消', inputValidator: (s) => (!!s || '请填键名') })
    name = (r as { value: string }).value
  } catch { return }
  if (name === props.rkey.name && !props.rkey.b64) return
  if (await edit({ op: 'rename', newKey: { t: name } }, '已改名')) emit('renamed', { name, type: props.rkey.type })
}
async function removeKey() {
  try {
    await ElMessageBox.confirm(`删除键「${props.rkey.name}」？删了就找不回来。`, '删除键', { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消', confirmButtonClass: 'el-button--danger' })
  } catch { return }
  if (await edit({ op: 'del', keys: [rstr(props.rkey)] }, '已删除')) emit('deleted')
}
async function copyName() {
  await navigator.clipboard.writeText(props.rkey.name)
  ElMessage.success({ message: '已复制', duration: 1200 })
}
const ttlEdit = reactive({ open: false, sec: 3600 })
async function setTTL(persist: boolean) {
  ttlEdit.open = false
  if (await edit(persist ? { op: 'persist' } : { op: 'expire', ttl: ttlEdit.sec }, persist ? '已设成永不过期' : '已设过期时间')) void load()
}

// ---------------------------------------------------------------- 一项一项的
const t = computed(() => v.value?.type ?? props.rkey.type)
const pairsText = (it: RedisItem) => {
  const p = it.p ?? []
  const out: string[] = []
  for (let i = 0; i + 1 < p.length; i += 2) out.push(`${p[i].t}=${p[i + 1].t}`)
  return out.join('  ')
}
function clip(x?: RedisVal) {
  if (!x) return ''
  const s = x.t.length > 300 ? x.t.slice(0, 300) + '…' : x.t
  return s.replace(/\n/g, '↵ ')
}
function itemEditable(it: RedisItem): boolean {
  switch (t.value) {
    case 'hash': return editable(it.f) && editable(it.v)
    case 'list': return editable(it.v)
    case 'set': case 'zset': return editable(it.f)
    default: return false
  }
}
/** 能删：用来指定它的字段、成员、值没被截断（二进制的有 base64） */
function itemRemovable(it: RedisItem): boolean {
  const id = t.value === 'list' ? it.v : it.f
  return !!id && !id.size
}

const dlg = reactive({ open: false, mode: 'add' as 'add' | 'edit' | 'view', item: null as RedisItem | null, field: '', value: '', score: '0', head: false, pairs: [{ k: '', v: '' }], busy: false })
function openItem(mode: 'add' | 'edit' | 'view', it: RedisItem | null = null, head = false) {
  Object.assign(dlg, { open: true, mode, item: it, head, busy: false, field: it?.f?.t ?? '', value: (t.value === 'list' ? it?.v?.t : it?.v?.t) ?? '', score: it?.s ?? '0', pairs: [{ k: '', v: '' }] })
  if (t.value === 'stream' && it) {
    const p = it.p ?? []
    dlg.pairs = []
    for (let i = 0; i + 1 < p.length; i += 2) dlg.pairs.push({ k: p[i].t, v: p[i + 1].t })
  }
}
function formatDlgJSON() {
  const p = pretty(dlg.value)
  if (p === null) return ElMessage.warning('不是 JSON')
  dlg.value = p
}
async function saveItem() {
  const it = dlg.item
  const add = dlg.mode === 'add'
  let e: RedisEdit
  switch (t.value) {
    case 'hash':
      if (!dlg.field) return ElMessage.warning('请填字段')
      e = { op: 'hset', field: { t: dlg.field }, value: dlg.value, ...(add || !it?.f ? {} : { orig: rstr(it.f) }) }
      break
    case 'list':
      e = add ? { op: 'push', value: dlg.value, head: dlg.head } : { op: 'lset', index: it!.i, orig: rstr(it!.v!), value: dlg.value }
      break
    case 'set':
      if (!dlg.field) return ElMessage.warning('请填成员')
      e = { op: 'sadd', field: { t: dlg.field }, ...(add || !it?.f ? {} : { orig: rstr(it.f) }) }
      break
    case 'zset':
      if (!dlg.field) return ElMessage.warning('请填成员')
      e = { op: 'zadd', field: { t: dlg.field }, score: dlg.score, ...(add || !it?.f ? {} : { orig: rstr(it.f) }) }
      break
    case 'stream': {
      const ps = dlg.pairs.filter((p) => p.k)
      if (!ps.length) return ElMessage.warning('请至少填一个字段')
      e = { op: 'xadd', pairs: ps.flatMap((p) => [p.k, p.v]) }
      break
    }
    default:
      return
  }
  dlg.busy = true
  const ok = await edit(e, add ? '已添加' : '已保存')
  dlg.busy = false
  if (ok) {
    dlg.open = false
    void load()
  }
}
async function removeItem(it: RedisItem) {
  const what = t.value === 'hash' ? `字段「${it.f?.t}」` : t.value === 'list' ? `第 ${(it.i ?? 0) + 1} 项` : t.value === 'stream' ? `消息 ${it.f?.t}` : `成员「${it.f?.t}」`
  try {
    await ElMessageBox.confirm(`删除${what}？`, '删除', { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消' })
  } catch { return }
  const op = { hash: 'hdel', list: 'ldel', set: 'srem', zset: 'zrem', stream: 'xdel' }[t.value]
  if (!op) return
  const e: RedisEdit = t.value === 'list' ? { op, index: it.i, orig: rstr(it.v!) } : { op, field: rstr(it.f!) }
  if (await edit(e, '已删除')) {
    // 剩下只有它一项时键也没了
    if ((v.value?.size ?? 0) <= 1) return emit('deleted')
    void load()
  }
}

const canMatch = computed(() => ['hash', 'set', 'zset'].includes(t.value))
let mt = 0
function onMatch() {
  clearTimeout(mt)
  mt = window.setTimeout(() => void load(), 350)
}
const addLabel = computed(() => ({ hash: '加字段', list: '', set: '加成员', zset: '加成员', stream: '加消息' }[t.value] ?? ''))
const dlgTitle = computed(() => {
  const what = { hash: '字段', list: '列表项', set: '成员', zset: '成员', stream: '消息' }[t.value] ?? ''
  return dlg.mode === 'add' ? (t.value === 'list' ? (dlg.head ? '加到列表开头' : '加到列表末尾') : `加${what}`) : dlg.mode === 'view' ? `查看${what}` : `修改${what}`
})

defineExpose({ hasChanges: () => (v.value?.type === 'string' ? dirtyJSON.value : false) })
</script>

<template>
  <div class="rk">
    <div class="head">
      <span class="tt" :class="t">{{ redisTypes[t] ?? t }}</span>
      <span class="name" :title="rkey.name">{{ rkey.name }}</span>
      <el-button link size="small" :icon="CopyDocument" title="复制键名" @click="copyName" />
      <span class="grow" />
      <el-button size="small" :icon="Refresh" :loading="loading" @click="load()">刷新</el-button>
      <el-button size="small" :icon="EditPen" :disabled="ro" @click="rename">重命名</el-button>
      <el-button size="small" :icon="Delete" type="danger" plain :disabled="ro" @click="removeKey">删除</el-button>
    </div>
    <div v-if="v && v.exists" class="meta">
      <el-popover v-model:visible="ttlEdit.open" trigger="click" placement="bottom-start" :width="280" :disabled="ro">
        <template #reference>
          <span class="ttl" :class="{ ro }" :title="ro ? '' : '点这里改过期时间'"><el-icon><Timer /></el-icon>{{ ttlText(v.ttl) }}</span>
        </template>
        <div class="ttl-box">
          <div class="row">
            <el-input-number v-model="ttlEdit.sec" :min="1" :controls="false" size="small" class="sec" />
            <span>秒后过期</span>
          </div>
          <div class="row quick">
            <el-button v-for="q in [[60, '1 分钟'], [3600, '1 小时'], [86400, '1 天'], [604800, '7 天']]" :key="q[0]" size="small" link @click="ttlEdit.sec = q[0] as number">{{ q[1] }}</el-button>
          </div>
          <div class="row">
            <el-button size="small" @click="setTTL(true)">永不过期</el-button>
            <span class="grow" />
            <el-button size="small" type="primary" @click="setTTL(false)">设置</el-button>
          </div>
        </div>
      </el-popover>
      <span>{{ t === 'string' ? humanSize(v.size) : `${v.size.toLocaleString()} 项` }}</span>
      <span v-if="v.memory">占内存 {{ humanSize(v.memory) }}</span>
      <span v-if="v.encoding" class="enc">{{ v.encoding }}</span>
    </div>
    <div v-if="error" class="err">{{ error }}</div>
    <div v-else-if="v && !v.exists" class="gone">这个键已经不在了（被删了或者过期了）。</div>

    <!-- 字符串 -->
    <template v-else-if="v && t === 'string'">
      <div class="sbar">
        <el-radio-group :model-value="view" size="small" @update:model-value="(m: string | number | boolean | undefined) => setView(m as 'text' | 'json')">
          <el-radio-button value="text">文本</el-radio-button>
          <el-radio-button value="json" :disabled="!jsonOK && view !== 'json'">JSON 格式化</el-radio-button>
        </el-radio-group>
        <span v-if="v.value?.bin" class="note">二进制的值，显示的是转义后的样子，不能在这里改</span>
        <span v-else-if="v.value?.size" class="note">值太大（{{ humanSize(v.value.size) }}），只显示了前 1 MB，不能在这里改</span>
        <span class="grow" />
        <template v-if="!ro && editable(v.value)">
          <el-button size="small" :disabled="!dirtyJSON" @click="text = orig; view = 'text'">撤销改动</el-button>
          <el-button size="small" type="primary" :disabled="!dirtyJSON" @click="saveString">保存</el-button>
        </template>
      </div>
      <el-input v-model="text" type="textarea" class="sval mono" :readonly="ro || !editable(v.value)" resize="none" spellcheck="false" />
    </template>

    <!-- 其他类型 -->
    <template v-else-if="v && ['hash', 'list', 'set', 'zset', 'stream'].includes(t)">
      <div class="sbar">
        <el-input v-if="canMatch" v-model="match" size="small" clearable :prefix-icon="Search" class="match" :placeholder="t === 'hash' ? '筛选字段' : '筛选成员'" @input="onMatch" @clear="onMatch" />
        <el-radio-group v-if="t === 'zset' && !match" v-model="desc" size="small" @change="load()">
          <el-radio-button :value="false">分数从小到大</el-radio-button>
          <el-radio-button :value="true">从大到小</el-radio-button>
        </el-radio-group>
        <span v-if="t === 'stream'" class="note">从新到旧</span>
        <span class="grow" />
        <template v-if="!ro">
          <template v-if="t === 'list'">
            <el-button size="small" :icon="Plus" @click="openItem('add', null, true)">加到开头</el-button>
            <el-button size="small" :icon="Plus" @click="openItem('add', null, false)">加到末尾</el-button>
          </template>
          <el-button v-else size="small" :icon="Plus" @click="openItem('add')">{{ addLabel }}</el-button>
        </template>
      </div>
      <el-table :data="items" size="small" border class="items" height="100%" empty-text="没有内容" @row-dblclick="(it: RedisItem) => openItem(!ro && itemEditable(it) ? 'edit' : 'view', it)">
        <el-table-column v-if="t === 'list' || (t === 'zset' && !match)" label="#" width="70">
          <template #default="{ row }">{{ (row.i ?? 0) + 1 }}</template>
        </el-table-column>
        <el-table-column v-if="t === 'stream'" label="ID" width="190">
          <template #default="{ row }"><span class="mono">{{ row.f?.t }}</span></template>
        </el-table-column>
        <el-table-column v-if="t === 'hash'" label="字段" min-width="120">
          <template #default="{ row }"><span class="mono" :class="{ bin: row.f?.bin }">{{ clip(row.f) }}</span></template>
        </el-table-column>
        <el-table-column v-if="t === 'set' || t === 'zset'" label="成员" min-width="160">
          <template #default="{ row }"><span class="mono" :class="{ bin: row.f?.bin }">{{ clip(row.f) }}</span></template>
        </el-table-column>
        <el-table-column v-if="t === 'zset'" label="分数" width="140">
          <template #default="{ row }"><span class="mono">{{ row.s }}</span></template>
        </el-table-column>
        <el-table-column v-if="t === 'hash' || t === 'list'" label="值" min-width="180">
          <template #default="{ row }"><span class="mono" :class="{ bin: row.v?.bin }">{{ clip(row.v) }}</span><span v-if="row.v?.size" class="cut">（{{ humanSize(row.v.size) }}，只显示了前面一部分）</span></template>
        </el-table-column>
        <el-table-column v-if="t === 'stream'" label="内容" min-width="200">
          <template #default="{ row }"><span class="mono">{{ pairsText(row) }}</span></template>
        </el-table-column>
        <el-table-column label="" width="80" align="right" fixed="right">
          <template #default="{ row }">
            <el-button v-if="t !== 'stream'" link size="small" :icon="EditPen" :title="!ro && itemEditable(row) ? '修改' : '查看'" @click="openItem(!ro && itemEditable(row) ? 'edit' : 'view', row)" />
            <el-button v-else link size="small" :icon="Search" title="查看" @click="openItem('view', row)" />
            <el-button link size="small" type="danger" :icon="Delete" title="删除" :disabled="ro || !itemRemovable(row)" @click="removeItem(row)" />
          </template>
        </el-table-column>
      </el-table>
      <div class="foot">
        <span>{{ items.length.toLocaleString() }} / {{ v.size.toLocaleString() }} 项<template v-if="match">（筛选后）</template></span>
        <el-button v-if="v.more" size="small" link type="primary" :loading="loading" @click="load(true)">加载更多</el-button>
        <span class="grow" />
        <span class="hint">双击一行查看或修改</span>
      </div>
    </template>
    <div v-else-if="v" class="gone">不支持查看这种类型（{{ t }}），可以在命令行里用相应的命令。</div>

    <el-dialog v-model="dlg.open" :title="dlgTitle" width="560px" append-to-body :close-on-click-modal="dlg.mode === 'view'">
      <el-form label-width="60px" @submit.prevent>
        <template v-if="t === 'stream'">
          <el-form-item v-if="dlg.item" label="ID"><span class="mono">{{ dlg.item.f?.t }}</span></el-form-item>
          <el-form-item label="内容">
            <div class="pairs">
              <div v-for="(p, i) in dlg.pairs" :key="i" class="pair">
                <el-input v-model="p.k" size="small" placeholder="字段" class="mono" :readonly="dlg.mode === 'view'" />
                <el-input v-model="p.v" size="small" placeholder="值" class="mono" :readonly="dlg.mode === 'view'" />
                <el-button v-if="dlg.mode !== 'view'" link size="small" :icon="Delete" :disabled="dlg.pairs.length === 1" @click="dlg.pairs.splice(i, 1)" />
              </div>
              <el-button v-if="dlg.mode !== 'view'" size="small" link :icon="Plus" @click="dlg.pairs.push({ k: '', v: '' })">加字段</el-button>
            </div>
          </el-form-item>
        </template>
        <template v-else>
          <el-form-item v-if="t === 'list' && dlg.item" label="位置">第 {{ (dlg.item.i ?? 0) + 1 }} 项</el-form-item>
          <el-form-item v-if="t !== 'list'" :label="t === 'hash' ? '字段' : '成员'">
            <el-input v-model="dlg.field" class="mono" :readonly="dlg.mode === 'view'" :type="t === 'hash' ? 'text' : 'textarea'" :autosize="{ minRows: 1, maxRows: 8 }" />
          </el-form-item>
          <el-form-item v-if="t === 'zset'" label="分数">
            <el-input v-model="dlg.score" class="mono" :readonly="dlg.mode === 'view'" />
          </el-form-item>
          <el-form-item v-if="t === 'hash' || t === 'list'" label="值">
            <el-input v-model="dlg.value" type="textarea" :rows="10" class="mono" :readonly="dlg.mode === 'view'" />
            <el-button size="small" link class="fmt" @click="formatDlgJSON">JSON 格式化</el-button>
          </el-form-item>
          <div v-if="dlg.mode === 'edit' && t !== 'list' && t !== 'hash'" class="tip">改成员会删掉原来的、加上新的。</div>
          <div v-if="dlg.mode === 'edit' && t === 'hash'" class="tip">改了字段名会把原来的字段改名。</div>
        </template>
      </el-form>
      <template #footer>
        <template v-if="dlg.mode === 'view'">
          <el-button @click="dlg.open = false">关闭</el-button>
        </template>
        <template v-else>
          <el-button @click="dlg.open = false">取消</el-button>
          <el-button type="primary" :loading="dlg.busy" @click="saveItem">{{ dlg.mode === 'add' ? '添加' : '保存' }}</el-button>
        </template>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.rk {
  height: 100%;
  display: flex;
  flex-direction: column;
  min-height: 0;
  padding: 8px 12px 10px;
  gap: 6px;
}
.head,
.sbar,
.meta,
.foot {
  display: flex;
  align-items: center;
  gap: 8px;
}
.head .el-button + .el-button,
.sbar .el-button + .el-button {
  margin-left: 0;
}
.name {
  font-family: 'Cascadia Mono', Consolas, monospace;
  font-weight: 600;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  min-width: 0;
}
.grow {
  flex: 1;
}
.tt {
  flex-shrink: 0;
  font-size: 11px;
  padding: 1px 6px;
  border-radius: 4px;
  color: #fff;
  background: var(--el-color-info);
}
.tt.string { background: #2f80ed; }
.tt.hash { background: #9b51e0; }
.tt.list { background: #27ae60; }
.tt.set { background: #e2a03f; }
.tt.zset { background: #eb5757; }
.tt.stream { background: #16a2b8; }
.meta {
  font-size: 12px;
  color: var(--el-text-color-secondary);
  gap: 14px;
}
.ttl {
  display: inline-flex;
  align-items: center;
  gap: 3px;
  cursor: pointer;
  color: var(--el-color-primary);
}
.ttl.ro {
  cursor: default;
  color: inherit;
}
.enc {
  font-family: 'Cascadia Mono', Consolas, monospace;
}
.ttl-box .row {
  display: flex;
  align-items: center;
  gap: 6px;
  margin-bottom: 6px;
}
.ttl-box .quick {
  gap: 2px;
}
.ttl-box .quick .el-button + .el-button {
  margin-left: 6px;
}
.sec {
  width: 120px;
}
.err {
  color: var(--el-color-danger);
  font-size: 13px;
  white-space: pre-wrap;
}
.gone {
  color: var(--el-text-color-secondary);
  padding: 20px 0;
}
.note,
.hint,
.cut,
.tip {
  font-size: 12px;
  color: var(--el-text-color-secondary);
}
.cut {
  margin-left: 4px;
}
.tip {
  padding-left: 60px;
}
.sval {
  flex: 1;
  min-height: 0;
}
.sval :deep(textarea) {
  height: 100% !important;
  font-size: 12.5px;
  line-height: 1.55;
}
.mono,
.mono :deep(input),
.mono :deep(textarea) {
  font-family: 'Cascadia Mono', Consolas, monospace;
}
.bin {
  color: var(--el-color-warning);
}
.match {
  width: 220px;
}
.items {
  flex: 1;
  min-height: 0;
}
.items .mono {
  font-size: 12.5px;
  white-space: nowrap;
}
.foot {
  font-size: 12px;
  color: var(--el-text-color-secondary);
}
.fmt {
  margin-top: 4px;
}
.pairs {
  width: 100%;
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.pair {
  display: flex;
  gap: 6px;
  align-items: center;
}
</style>
