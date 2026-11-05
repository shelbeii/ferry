<script setup lang="ts">
// Redis 的一个库：左边是键（按「:」分组成树，用 SCAN 列，不会卡住 Redis），右边是选中的键的值。
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Refresh, Plus, Search, Monitor, Folder, Key, Delete } from '@element-plus/icons-vue'
import DbRedisKey from './DbRedisKey.vue'
import { closeOnOutside, dbCall, redisTypes, rstr, type DbConn, type RedisEdit, type RedisKey, type RedisKeys } from '../../db'

const props = defineProps<{ conn: DbConn; db: string }>()
const emit = defineEmits<{ console: []; changed: [] }>()

const ro = computed(() => !!props.conn.readOnly)

// ---------------------------------------------------------------- 列键
const pattern = ref('')
const type = ref('')
const sep = ref(':')
const keys = ref<RedisKey[]>([])
const cursor = ref('0')
const total = ref(0)
const loading = ref(false)
const error = ref('')

function globOf(s: string) {
  s = s.trim()
  if (!s) return '*'
  return /[*?[]/.test(s) ? s : `*${s}*`
}

async function load(more = false) {
  loading.value = true
  error.value = ''
  try {
    const r = await dbCall<RedisKeys>(props.conn, 'redis-keys', props.db, { scan: { pattern: globOf(pattern.value), type: type.value, cursor: more ? cursor.value : '', count: 1000 } })
    if (more) {
      const seen = new Set(keys.value.map(keyId))
      keys.value = [...keys.value, ...r.keys.filter((k) => !seen.has(keyId(k)))]
    } else {
      keys.value = r.keys
    }
    cursor.value = r.cursor
    total.value = r.total
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}
onMounted(() => void load())
let timer = 0
watch([pattern, type], () => {
  clearTimeout(timer)
  timer = window.setTimeout(() => void load(), 350)
})

function keyId(k: RedisKey) {
  return 'k:' + (k.b64 ? 'b:' + k.b64 : k.name)
}

// ---------------------------------------------------------------- 树
interface KNode { id: string; label: string; children?: KNode[]; key?: RedisKey; count?: number; prefix?: string }

const nodes = computed<KNode[]>(() => {
  const root: KNode[] = []
  const folders = new Map<string, KNode>()
  for (const k of keys.value) {
    const parts = sep.value ? k.name.split(sep.value) : [k.name]
    let list = root
    let prefix = ''
    for (let i = 0; i < parts.length - 1; i++) {
      prefix += parts[i] + sep.value
      let f = folders.get(prefix)
      if (!f) {
        f = { id: 'f:' + prefix, label: parts[i] || '（空）', children: [], count: 0, prefix }
        folders.set(prefix, f)
        list.push(f)
      }
      f.count!++
      list = f.children!
    }
    list.push({ id: keyId(k), label: parts[parts.length - 1] || '（空）', key: k })
  }
  const sort = (l: KNode[]) => {
    l.sort((a, b) => Number(!!a.key) - Number(!!b.key) || a.label.localeCompare(b.label))
    for (const n of l) if (n.children) sort(n.children)
  }
  sort(root)
  return root
})

const tree = ref()
const expanded = new Set<string>()
watch(nodes, async () => {
  await nextTick()
  tree.value?.setExpandedKeys?.([...expanded])
  if (current.value) tree.value?.setCurrentKey?.(keyId(current.value))
})

const box = ref<HTMLElement>()
const height = ref(400)
let ro2: ResizeObserver | null = null
onMounted(() => {
  if (!box.value) return
  ro2 = new ResizeObserver(() => { height.value = Math.max(100, box.value?.clientHeight ?? 400) })
  ro2.observe(box.value)
})
onBeforeUnmount(() => { ro2?.disconnect(); offMenu?.() })

const current = ref<RedisKey | null>(null)
const detail = ref<InstanceType<typeof DbRedisKey>>()
async function pick(k: RedisKey) {
  if (current.value && keyId(current.value) === keyId(k)) return
  if (detail.value?.hasChanges?.()) {
    try {
      await ElMessageBox.confirm('这个键的值改了还没保存，换到别的键就没了。', '没保存', { type: 'warning', confirmButtonText: '不保存', cancelButtonText: '回去保存' })
    } catch {
      tree.value?.setCurrentKey?.(keyId(current.value!))
      return
    }
  }
  current.value = k
}
function onNodeClick(d: KNode) {
  if (d.key) void pick(d.key)
}
function typeShort(t: string) {
  return { string: 'STR', hash: 'HASH', list: 'LIST', set: 'SET', zset: 'ZSET', stream: 'XS' }[t] ?? t.slice(0, 4).toUpperCase()
}

// ---------------------------------------------------------------- 右键菜单
const menu = ref<{ x: number; y: number; d: KNode } | null>(null)
const menuEl = ref<HTMLElement>()
let offMenu: (() => void) | null = null
function onContext(e: MouseEvent, d: KNode) {
  e.preventDefault()
  offMenu?.()
  menu.value = { x: e.clientX, y: e.clientY, d }
  offMenu = closeOnOutside((t) => !!menuEl.value?.contains(t), () => { menu.value = null })
  void nextTick(() => {
    const el = menuEl.value
    const m = menu.value
    if (!el || !m) return
    const r = el.getBoundingClientRect()
    m.x = Math.max(4, Math.min(m.x, window.innerWidth - r.width - 4))
    m.y = Math.max(4, Math.min(m.y, window.innerHeight - r.height - 4))
  })
}
const menuItems = computed(() => {
  const d = menu.value?.d
  if (!d) return []
  if (d.key) return [{ k: 'open', t: '打开' }, { k: 'copy', t: '复制键名' }, { k: 'del', t: '删除这个键…', danger: true, off: ro.value, sep: true }]
  return [
    { k: 'new', t: '在这一组里新建键…', off: ro.value }, { k: 'copy', t: '复制前缀' }, { k: 'filter', t: '只看这一组' },
    { k: 'del-group', t: `删除这一组列出的 ${d.count} 个键…`, danger: true, off: ro.value, sep: true },
  ]
})
function leaves(d: KNode): RedisKey[] {
  if (d.key) return [d.key]
  return (d.children ?? []).flatMap(leaves)
}
async function menuDo(k: string) {
  const d = menu.value?.d
  menu.value = null
  offMenu?.()
  if (!d) return
  switch (k) {
    case 'open': return d.key && pick(d.key)
    case 'copy':
      await navigator.clipboard.writeText(d.key ? d.key.name : d.prefix ?? '')
      return void ElMessage.success({ message: '已复制', duration: 1200 })
    case 'filter':
      pattern.value = (d.prefix ?? '') + '*'
      return
    case 'new': return openNew(d.prefix)
    case 'del': return d.key && removeKeys([d.key], `删除键「${d.key.name}」？删了就找不回来。`)
    case 'del-group': {
      const ks = leaves(d)
      const note = cursor.value !== '0' ? '\n\n还有没扫描到的键，它们不会被删。' : ''
      return removeKeys(ks, `删除「${d.prefix}」这一组列出的 ${ks.length} 个键？删了就找不回来。${note}`)
    }
  }
}

async function removeKeys(ks: RedisKey[], msg: string) {
  try {
    await ElMessageBox.confirm(msg, '删除键', { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消', confirmButtonClass: 'el-button--danger' })
  } catch { return }
  try {
    let n = 0
    for (let i = 0; i < ks.length; i += 500) {
      const r = await dbCall<{ n: number }>(props.conn, 'redis-edit', props.db, { edit: { op: 'del', keys: ks.slice(i, i + 500).map(rstr) } })
      n += r.n
    }
    ElMessage.success(`删了 ${n} 个键`)
    const gone = new Set(ks.map(keyId))
    keys.value = keys.value.filter((k) => !gone.has(keyId(k)))
    total.value = Math.max(0, total.value - n)
    if (current.value && gone.has(keyId(current.value))) current.value = null
    emit('changed')
  } catch (e) {
    void ElMessageBox.alert((e as Error).message, '没有删成', { type: 'error' })
  }
}

function onDeleted() {
  if (!current.value) return
  const id = keyId(current.value)
  keys.value = keys.value.filter((k) => keyId(k) !== id)
  total.value = Math.max(0, total.value - 1)
  current.value = null
  emit('changed')
}
function onRenamed(k: RedisKey) {
  const id = current.value ? keyId(current.value) : ''
  keys.value = [...keys.value.filter((x) => keyId(x) !== id), k]
  current.value = k
}

// ---------------------------------------------------------------- 新建键
const nk = reactive({ open: false, name: '', type: 'string', value: '', field: '', score: '0', ttl: 0, pairs: [{ k: '', v: '' }], busy: false })
function openNew(prefix = '') {
  Object.assign(nk, { open: true, name: prefix, type: 'string', value: '', field: '', score: '0', ttl: 0, pairs: [{ k: '', v: '' }], busy: false })
}
async function saveNew() {
  const name = nk.name
  if (!name.trim()) return ElMessage.warning('请填键名')
  const e: RedisEdit = { op: 'new', type: nk.type, key: { t: name }, ttl: nk.ttl > 0 ? nk.ttl : 0 }
  if (nk.type === 'string' || nk.type === 'list' || nk.type === 'hash') e.value = nk.value
  if (nk.type === 'hash' || nk.type === 'set' || nk.type === 'zset') {
    if (!nk.field) return ElMessage.warning(nk.type === 'hash' ? '请填字段' : '请填成员')
    e.field = { t: nk.field }
  }
  if (nk.type === 'zset') e.score = nk.score
  if (nk.type === 'stream') {
    const ps = nk.pairs.filter((p) => p.k)
    if (!ps.length) return ElMessage.warning('请至少填一个字段')
    e.pairs = ps.flatMap((p) => [p.k, p.v])
  }
  nk.busy = true
  try {
    await dbCall(props.conn, 'redis-edit', props.db, { edit: e })
    nk.open = false
    const k: RedisKey = { name, type: nk.type }
    if (!keys.value.some((x) => keyId(x) === keyId(k))) keys.value = [...keys.value, k]
    total.value++
    current.value = k
    // 新键所在的组展开
    let p = ''
    for (const part of sep.value ? name.split(sep.value).slice(0, -1) : []) {
      p += part + sep.value
      expanded.add('f:' + p)
    }
    emit('changed')
  } catch (err) {
    void ElMessageBox.alert((err as Error).message, '没有建成', { type: 'error' })
  } finally {
    nk.busy = false
  }
}

defineExpose({
  hasChanges: () => !!detail.value?.hasChanges?.(),
  reload: () => { current.value = null; void load() },
  newKey: () => openNew(),
})
</script>

<template>
  <div class="rv">
    <div class="bar">
      <el-input v-model="pattern" size="small" :prefix-icon="Search" clearable class="pat" placeholder="筛选键：如 user:*（不带 * 时按包含查找）" />
      <el-select v-model="type" size="small" class="type" placeholder="全部类型" clearable>
        <el-option v-for="(l, k) in redisTypes" :key="k" :value="k" :label="l" />
      </el-select>
      <el-tooltip content="按这个符号把键分组（空 = 不分组）" placement="bottom" :show-after="400">
        <el-input v-model="sep" size="small" class="sep"><template #prefix>分组</template></el-input>
      </el-tooltip>
      <el-button size="small" :icon="Refresh" :loading="loading" @click="load()">刷新</el-button>
      <el-button size="small" :icon="Plus" :disabled="ro" @click="openNew()">新建键</el-button>
      <el-button size="small" :icon="Monitor" @click="emit('console')">命令行</el-button>
    </div>
    <div class="body">
      <aside class="keys">
        <div class="stat">
          <span>{{ cursor === '0' ? `${keys.length.toLocaleString()} 个键` : `已列出 ${keys.length.toLocaleString()} 个` }}<template v-if="pattern || type">（筛选后）</template> · 库里一共 {{ total.toLocaleString() }} 个</span>
        </div>
        <div v-if="error" class="err">{{ error }}</div>
        <div ref="box" class="tree">
          <el-tree-v2
            v-if="keys.length"
            ref="tree"
            :data="nodes"
            :props="{ value: 'id', label: 'label', children: 'children' }"
            :height="height"
            :item-size="26"
            highlight-current
            :expand-on-click-node="true"
            @node-click="onNodeClick"
            @node-expand="(d: KNode) => expanded.add(d.id)"
            @node-collapse="(d: KNode) => expanded.delete(d.id)"
            @node-contextmenu="(e: Event, d: KNode) => onContext(e as MouseEvent, d)"
          >
            <template #default="{ data }">
              <span class="kn" :title="data.key ? data.key.name : data.prefix + '*'">
                <el-icon v-if="!data.key" class="fi"><Folder /></el-icon>
                <span v-else class="tt" :class="data.key.type">{{ typeShort(data.key.type) }}</span>
                <span class="kl">{{ data.label }}</span>
                <span v-if="!data.key" class="cnt">{{ data.count }}</span>
              </span>
            </template>
          </el-tree-v2>
          <div v-else-if="!loading && !error" class="none">{{ pattern || type ? '没有符合条件的键' : '这个库是空的' }}</div>
        </div>
        <div v-if="cursor !== '0'" class="more">
          <el-button size="small" link type="primary" :loading="loading" @click="load(true)">接着扫描更多键</el-button>
          <span class="hint">用 SCAN 分批列，不会卡住 Redis</span>
        </div>
      </aside>
      <section class="detail">
        <DbRedisKey v-if="current" :key="keyId(current)" ref="detail" :conn="conn" :db="db" :rkey="current" @deleted="onDeleted" @renamed="onRenamed" />
        <div v-else class="empty">
          <el-icon><Key /></el-icon>
          <p>左边点一个键看它的值</p>
        </div>
      </section>
    </div>

    <teleport to="body">
      <ul v-if="menu" ref="menuEl" class="db-grid-menu" :style="{ left: menu.x + 'px', top: menu.y + 'px' }" @click.stop>
        <li v-for="m in menuItems" :key="m.k" :class="{ sep: m.sep, danger: m.danger, off: m.off }" @click="!m.off && menuDo(m.k)">{{ m.t }}</li>
      </ul>
    </teleport>

    <el-dialog v-model="nk.open" title="新建键" width="520px" append-to-body :close-on-click-modal="false">
      <el-form label-width="80px" @submit.prevent>
        <el-form-item label="键名">
          <el-input v-model="nk.name" placeholder="如 user:1001" class="mono" />
        </el-form-item>
        <el-form-item label="类型">
          <el-radio-group v-model="nk.type" size="small">
            <el-radio-button v-for="(l, k) in redisTypes" :key="k" :value="k">{{ l }}</el-radio-button>
          </el-radio-group>
        </el-form-item>
        <el-form-item v-if="nk.type === 'hash'" label="字段">
          <el-input v-model="nk.field" class="mono" />
        </el-form-item>
        <el-form-item v-if="nk.type === 'set' || nk.type === 'zset'" label="成员">
          <el-input v-model="nk.field" class="mono" />
        </el-form-item>
        <el-form-item v-if="nk.type === 'zset'" label="分数">
          <el-input v-model="nk.score" class="mono" placeholder="数字，如 10、-1.5" />
        </el-form-item>
        <el-form-item v-if="nk.type === 'string' || nk.type === 'hash' || nk.type === 'list'" :label="nk.type === 'list' ? '第一项' : '值'">
          <el-input v-model="nk.value" type="textarea" :rows="5" class="mono" />
        </el-form-item>
        <el-form-item v-if="nk.type === 'stream'" label="消息">
          <div class="pairs">
            <div v-for="(p, i) in nk.pairs" :key="i" class="pair">
              <el-input v-model="p.k" size="small" placeholder="字段" class="mono" />
              <el-input v-model="p.v" size="small" placeholder="值" class="mono" />
              <el-button link size="small" :icon="Delete" :disabled="nk.pairs.length === 1" @click="nk.pairs.splice(i, 1)" />
            </div>
            <el-button size="small" link :icon="Plus" @click="nk.pairs.push({ k: '', v: '' })">加字段</el-button>
          </div>
        </el-form-item>
        <el-form-item label="过期">
          <el-input-number v-model="nk.ttl" :min="0" :controls="false" class="ttl" />
          <span class="unit">秒后过期（0 = 永不过期）</span>
        </el-form-item>
        <div class="tip">还有别的字段、成员的话，建好后在右边接着加。</div>
      </el-form>
      <template #footer>
        <el-button @click="nk.open = false">取消</el-button>
        <el-button type="primary" :loading="nk.busy" @click="saveNew">新建</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.rv {
  height: 100%;
  display: flex;
  flex-direction: column;
  min-height: 0;
}
.bar {
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 8px 10px;
  flex-wrap: wrap;
  border-bottom: 1px solid var(--el-border-color-lighter);
}
.bar .el-button + .el-button {
  margin-left: 0;
}
.pat {
  width: 300px;
}
.type {
  width: 120px;
}
.sep {
  width: 90px;
}
.body {
  flex: 1;
  min-height: 0;
  display: flex;
}
.keys {
  width: 340px;
  flex-shrink: 0;
  display: flex;
  flex-direction: column;
  border-right: 1px solid var(--el-border-color-lighter);
  min-height: 0;
}
.stat {
  padding: 6px 10px;
  font-size: 12px;
  color: var(--el-text-color-secondary);
}
.err {
  margin: 0 10px 6px;
  color: var(--el-color-danger);
  font-size: 12.5px;
  white-space: pre-wrap;
}
.tree {
  flex: 1;
  min-height: 0;
  overflow: hidden;
}
.tree :deep(.el-tree) {
  background: transparent;
  font-size: 13px;
}
.kn {
  display: flex;
  align-items: center;
  gap: 6px;
  min-width: 0;
  flex: 1;
  padding-right: 8px;
}
.fi {
  color: var(--el-text-color-secondary);
  flex-shrink: 0;
}
.kl {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-family: 'Cascadia Mono', Consolas, monospace;
  font-size: 12.5px;
}
.cnt {
  margin-left: auto;
  font-size: 11px;
  color: var(--el-text-color-placeholder);
}
.tt {
  flex-shrink: 0;
  font-size: 9.5px;
  line-height: 14px;
  width: 34px;
  text-align: center;
  border-radius: 3px;
  font-weight: 600;
  color: #fff;
  background: var(--el-color-info);
}
.tt.string { background: #2f80ed; }
.tt.hash { background: #9b51e0; }
.tt.list { background: #27ae60; }
.tt.set { background: #e2a03f; }
.tt.zset { background: #eb5757; }
.tt.stream { background: #16a2b8; }
.none {
  padding: 30px 12px;
  text-align: center;
  color: var(--el-text-color-placeholder);
  font-size: 13px;
}
.more {
  padding: 6px 10px;
  border-top: 1px solid var(--el-border-color-lighter);
  display: flex;
  align-items: center;
  gap: 8px;
}
.hint,
.tip,
.unit {
  font-size: 12px;
  color: var(--el-text-color-secondary);
}
.unit {
  margin-left: 8px;
}
.tip {
  padding-left: 80px;
}
.detail {
  flex: 1;
  min-width: 0;
  min-height: 0;
  display: flex;
  flex-direction: column;
}
.empty {
  margin: auto;
  text-align: center;
  color: var(--el-text-color-placeholder);
}
.empty .el-icon {
  font-size: 34px;
}
.mono :deep(input),
.mono :deep(textarea) {
  font-family: 'Cascadia Mono', Consolas, monospace;
}
.ttl {
  width: 120px;
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
