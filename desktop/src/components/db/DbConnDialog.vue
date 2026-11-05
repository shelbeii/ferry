<script setup lang="ts">
// 新建、修改数据库连接：这台电脑直接连，或经项目里的一台服务器（SSH 转发）连；经服务器时可以从那台服务器的「软件」里直接选数据库。
import { computed, reactive, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { Connection, Search } from '@element-plus/icons-vue'
import GlassToggle from '../glass/GlassToggle.vue'
import { call, type Inspect, type SoftwareReport, type Software } from '../../ferry'
import { projects, connections, connectionsPath } from '../../state'
import { saveDbConn, testDbConn, connFromSoftware, isDbSoftware, defaultPort, defaultUser, kindLabel, type DbConn, type DbKind } from '../../db'

const props = defineProps<{ conn: Partial<DbConn> | null }>()
const emit = defineEmits<{ close: []; saved: [conn: DbConn] }>()

const editing = computed(() => !!props.conn?.id)
const blank = () => ({
  name: '',
  kind: 'mysql' as DbKind,
  via: false,
  server: '' as string, // config \n target
  host: '127.0.0.1',
  port: 3306,
  user: 'root',
  password: '',
  savePassword: true,
  database: '',
  readOnly: false,
  ai: false,
  aiWrite: false,
  container: '',
  software: '',
})
const f = reactive(blank())
const nameTouched = ref(false)
const busy = ref(false)
const testing = ref(false)
const testMsg = ref<{ ok: boolean; text: string } | null>(null)

// 服务器：项目里的目标 + 「新建连接」
interface ServerOpt { value: string; label: string; production?: boolean }
const groups = ref<{ label: string; options: ServerOpt[] }[]>([])
const loadingServers = ref(false)
async function loadServers() {
  loadingServers.value = true
  const out: { label: string; options: ServerOpt[] }[] = []
  await Promise.all(projects.value.map(async (p) => {
    try {
      const ins = await call<Inspect>(['inspect', '-c', p.path])
      out.push({ label: `项目 ${ins.project || p.name}`, options: ins.targets.map((t) => ({ value: `${p.path}\n${t.name}`, label: `${t.name}（${t.user}@${t.host}）`, production: t.production })) })
    } catch {
      /* 读不了的项目跳过 */
    }
  }))
  out.sort((a, b) => a.label.localeCompare(b.label))
  if (connections.value.length && connectionsPath.value) {
    out.push({ label: '新建连接', options: connections.value.map((c) => ({ value: `${connectionsPath.value}\n${c.name}`, label: `${c.title || c.name}（${c.user}@${c.host}）` })) })
  }
  groups.value = out
  loadingServers.value = false
}
const serverOpt = computed(() => groups.value.flatMap((g) => g.options).find((o) => o.value === f.server))
const serverTitle = computed(() => serverOpt.value?.label.replace(/（.*$/, '') ?? '')

// 从服务器上的软件里选
const swLoading = ref(false)
const swError = ref('')
const swItems = ref<Software[] | null>(null)
async function loadSoftware() {
  if (!f.server) return
  const [config, target] = f.server.split('\n')
  swLoading.value = true
  swError.value = ''
  try {
    const r = await call<SoftwareReport>(['software', target, '-c', config])
    swItems.value = r.items.filter(isDbSoftware)
    if (!swItems.value.length) swError.value = '这台服务器上没找到 MySQL、MariaDB、PostgreSQL、Redis'
  } catch (e) {
    swError.value = (e as Error).message
  } finally {
    swLoading.value = false
  }
}
function pickSoftware(sw: Software) {
  const [config, target] = f.server.split('\n')
  const c = connFromSoftware(sw, { config, target, title: serverTitle.value }, serverTitle.value || target)
  if (!c) return
  fill(c)
  swItems.value = null
  testMsg.value = null
}
function swLabel(sw: Software) {
  const where = sw.listen?.length ? `端口 ${sw.listen.map((l) => l.slice(l.lastIndexOf(':') + 1)).join('、')}` : sw.inner?.length ? `容器内端口 ${sw.inner.map((l) => l.slice(l.lastIndexOf(':') + 1)).join('、')}` : '没有监听端口'
  return `${sw.name}${sw.version ? ' ' + sw.version : ''} · ${sw.via === 'docker' ? '容器 ' + sw.container : sw.unit || sw.via} · ${where}${sw.manager ? ' · ' + sw.manager : ''}${sw.state === 'stopped' ? ' · 没在运行' : ''}`
}

function fill(c: Partial<DbConn>) {
  if (c.kind) f.kind = c.kind
  if (c.name !== undefined) f.name = c.name
  f.via = !!c.via
  f.server = c.via ? `${c.via.config}\n${c.via.target}` : f.server
  if (c.host !== undefined) f.host = c.host
  if (c.port) f.port = c.port
  if (c.user) f.user = c.user
  f.database = c.database ?? f.database
  f.readOnly = c.readOnly ?? f.readOnly
  f.ai = c.ai ?? f.ai
  f.aiWrite = c.aiWrite ?? f.aiWrite
  f.savePassword = c.savePassword ?? f.savePassword
  f.container = c.container ?? ''
  f.software = c.software ?? ''
}

watch(() => props.conn, (c) => {
  testMsg.value = null
  f.password = ''
  nameTouched.value = !!c?.id
  Object.assign(f, blank())
  if (c) fill(c)
  if (c?.via || !c?.id) void loadServers()
}, { immediate: true })

watch(() => f.kind, (k, old) => {
  if (old && f.port === defaultPort[old]) f.port = defaultPort[k]
  if (old && f.user === defaultUser[old]) f.user = defaultUser[k]
  if (old && (k === 'redis') !== (old === 'redis')) f.database = ''
})
const isRedis = computed(() => f.kind === 'redis')
watch(() => f.server, () => {
  swItems.value = null
  swError.value = ''
  if (!editing.value && serverOpt.value?.production) f.readOnly = true
})
watch(() => f.via, (v) => { if (v && !groups.value.length) void loadServers() })

const autoName = computed(() => {
  const where = f.container ? `容器 ${f.container}` : `${f.host || '127.0.0.1'}:${f.port}`
  const who = isRedis.value ? (f.user ? `Redis ${f.user}@` : 'Redis ') : `${f.user}@`
  return f.via && serverTitle.value ? `${who}${where} · ${serverTitle.value}` : `${who}${where}`
})

function payload(): Partial<DbConn> {
  const [config, target] = f.server.split('\n')
  return {
    id: props.conn?.id,
    name: (nameTouched.value && f.name.trim()) || f.name.trim() || autoName.value,
    kind: f.kind,
    host: f.container ? '' : f.host.trim(),
    port: Number(f.port),
    user: f.user.trim(),
    database: f.database.trim() || undefined,
    via: f.via && f.server ? { config, target, title: serverTitle.value } : null,
    container: f.via ? f.container || undefined : undefined,
    software: f.via ? f.software || undefined : undefined,
    readOnly: f.readOnly,
    ai: f.ai,
    aiWrite: f.ai && !f.readOnly && f.aiWrite,
    savePassword: f.savePassword,
    color: props.conn?.color,
  }
}

function check(): string {
  if (f.via && !f.server) return '请选经哪台服务器连'
  if (!f.container && !f.host.trim()) return '请填地址'
  if (!f.user.trim() && !isRedis.value) return '请填用户名'
  if (isRedis.value && f.database.trim() && !/^(db)?\d+$/i.test(f.database.trim())) return '库编号要填数字（0、1、2……）'
  if (!(Number(f.port) > 0)) return '请填端口'
  return ''
}

async function test() {
  const err = check()
  if (err) return (testMsg.value = { ok: false, text: err })
  testing.value = true
  testMsg.value = null
  try {
    const r = await testDbConn(payload(), f.password || undefined)
    const keys = r.databases.reduce((n, d) => n + (d.keys ?? 0), 0)
    testMsg.value = { ok: true, text: isRedis.value ? `连上了：${r.version}，一共 ${keys.toLocaleString()} 个键` : `连上了：${r.version}，有 ${r.databases.length} 个库` }
  } catch (e) {
    testMsg.value = { ok: false, text: (e as Error).message }
  } finally {
    testing.value = false
  }
}

async function save() {
  const err = check()
  if (err) return (testMsg.value = { ok: false, text: err })
  busy.value = true
  try {
    const saved = await saveDbConn(payload(), f.password || undefined)
    ElMessage.success(editing.value ? '已保存' : '已添加')
    emit('saved', saved)
  } catch (e) {
    testMsg.value = { ok: false, text: (e as Error).message }
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <el-dialog :model-value="!!conn" :title="editing ? '修改数据库连接' : '新建数据库连接'" width="600px" append-to-body :close-on-click-modal="false" @close="emit('close')">
    <el-form label-width="96px" size="default" @submit.prevent>
      <el-form-item label="数据库">
        <el-radio-group v-model="f.kind">
          <el-radio-button v-for="(l, k) in kindLabel" :key="k" :value="k">{{ l }}</el-radio-button>
        </el-radio-group>
      </el-form-item>
      <el-form-item label="怎么连">
        <el-radio-group v-model="f.via">
          <el-radio :value="false">这台电脑直接连</el-radio>
          <el-radio :value="true">经服务器（SSH 转发，不用开放数据库端口）</el-radio>
        </el-radio-group>
      </el-form-item>
      <template v-if="f.via">
        <el-form-item label="服务器">
          <div class="row">
            <el-select v-model="f.server" filterable :loading="loadingServers" placeholder="选项目里的服务器或「新建连接」" class="grow">
              <el-option-group v-for="g in groups" :key="g.label" :label="g.label">
                <el-option v-for="o in g.options" :key="o.value" :value="o.value" :label="o.label">
                  <span>{{ o.label }}</span>
                  <el-tag v-if="o.production" size="small" type="danger" class="prod">生产</el-tag>
                </el-option>
              </el-option-group>
            </el-select>
            <el-button :icon="Search" :disabled="!f.server" :loading="swLoading" @click="loadSoftware">从服务器的软件里选</el-button>
          </div>
        </el-form-item>
        <el-form-item v-if="swItems || swError" label=" ">
          <div class="sw">
            <div v-if="swError" class="sw-err">{{ swError }}</div>
            <div v-for="(sw, i) in swItems ?? []" :key="i" class="sw-item" @click="pickSoftware(sw)">
              <el-icon><Connection /></el-icon>
              <span>{{ swLabel(sw) }}</span>
            </div>
          </div>
        </el-form-item>
      </template>
      <el-form-item :label="f.via ? '数据库地址' : '地址'">
        <div v-if="f.container" class="row">
          <span class="grow">容器 <b>{{ f.container }}</b> 里（端口没映射到服务器上，连接时查容器的地址）</span>
          <el-button link @click="f.container = ''; f.host = '127.0.0.1'">改成填地址</el-button>
        </div>
        <div v-else class="row">
          <el-input v-model="f.host" :placeholder="f.via ? '从服务器上看的地址，通常是 127.0.0.1' : 'IP 或域名'" class="grow" />
          <span>端口</span>
          <el-input-number v-model="f.port" :min="1" :max="65535" :controls="false" class="port" />
        </div>
        <div v-if="f.container" class="row mt">
          <span>容器内端口</span>
          <el-input-number v-model="f.port" :min="1" :max="65535" :controls="false" class="port" />
        </div>
      </el-form-item>
      <el-form-item label="用户名">
        <el-input v-model="f.user" :placeholder="isRedis ? '一般不填（Redis 6 起设了 ACL 用户才填）' : ''" />
      </el-form-item>
      <el-form-item label="密码">
        <el-input v-model="f.password" type="password" show-password :placeholder="editing ? '不改就留空' : f.software ? '可以不填：用 Ferry 安装时保存的密码' : '没有密码就留空'" autocomplete="new-password" />
        <GlassToggle v-model="f.savePassword" class="mt">记住密码（存在系统凭据库里）；不记住时每次打开 Ferry 后第一次连接要填</GlassToggle>
      </el-form-item>
      <el-form-item :label="isRedis ? '默认库编号' : '默认库'">
        <el-input v-model="f.database" :placeholder="isRedis ? '不填是 0（db0）' : f.kind === 'postgres' ? '不填连 postgres' : '可以不填'" />
      </el-form-item>
      <el-form-item label="只读">
        <GlassToggle v-model="f.readOnly">{{ isRedis ? '只许看，不许改键和值（命令行里也只能执行只读的命令）' : '只许查，不许改数据和表结构' }}</GlassToggle>
        <span v-if="serverOpt?.production" class="hint">（生产服务器，默认只读）</span>
      </el-form-item>
      <el-form-item label="AI">
        <GlassToggle v-model="f.ai">允许 AI 查询（编程软件经 MCP、Ferry 的 AI 助手）</GlassToggle>
        <div v-if="f.ai" class="note">
          <template v-if="isRedis">AI 能看有哪些键、执行只读的命令（GET、HGETALL、SCAN……，一次一条，最多 15 秒）；不能看配置和密码（CONFIG、ACL）。读到的数据会发给 AI 用的模型。</template>
          <template v-else>AI 能看库表结构、执行只读查询（一次一条，最多 15 秒、500 行）；查出来的数据会发给 AI 用的模型。</template>
          <template v-if="!f.savePassword && !f.software"><br /><b>要勾选「记住密码」</b>：AI 不会问密码，没记住时连不上。</template>
        </div>
        <GlassToggle v-if="f.ai" v-model="f.aiWrite" :disabled="f.readOnly">{{ isRedis ? '允许 AI 修改（加、改、删键和里面的数据）' : '允许 AI 修改（插入、修改、删除数据，建表改表）' }}</GlassToggle>
        <div v-if="f.ai && f.readOnly" class="note">连接设成了只读，AI 不能改。</div>
        <div v-else-if="f.ai && f.aiWrite" class="note">
          <template v-if="isRedis">每次都把完整的命令给你看，你同意后一起按顺序执行（MULTI/EXEC）。Redis 没有回滚：执行时出错的那条不生效，别的照常生效。不能清空库（FLUSHDB）、改配置、执行脚本。</template>
          <template v-else>每次都把完整的 SQL 给你看，你同意后才在一个事务里执行，失败全部撤回（MySQL 的建表改表撤不回）。不能建库删库、改账号权限。</template>
          编程软件要在「设置 → 命令行与 MCP」里允许请求改动；<template v-if="serverOpt?.production"><b>这是生产服务器：编程软件不能改，Ferry 的 AI 助手每次都要你确认。</b></template><template v-else>经生产服务器连的，编程软件不能改。</template>
        </div>
      </el-form-item>
      <el-form-item label="连接名">
        <el-input v-model="f.name" :placeholder="autoName" @input="nameTouched = true" />
      </el-form-item>
    </el-form>
    <el-alert v-if="testMsg" :type="testMsg.ok ? 'success' : 'error'" :closable="false" :title="testMsg.text" class="msg" />
    <template #footer>
      <el-button :loading="testing" @click="test">测试连接</el-button>
      <el-button @click="emit('close')">取消</el-button>
      <el-button type="primary" :loading="busy" @click="save">保存</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.row {
  display: flex;
  align-items: center;
  gap: 8px;
  width: 100%;
}
.grow {
  flex: 1;
  min-width: 0;
}
.port {
  width: 100px;
}
.mt {
  margin-top: 6px;
}
.prod {
  margin-left: 6px;
}
.note {
  width: 100%;
  line-height: 1.5;
  color: var(--el-text-color-secondary);
  font-size: 12.5px;
}
.hint {
  margin-left: 6px;
  color: var(--el-text-color-secondary);
  font-size: 12.5px;
}
.sw {
  width: 100%;
  border: 1px solid var(--el-border-color-lighter);
  border-radius: 6px;
  padding: 4px;
  max-height: 180px;
  overflow: auto;
}
.sw-err {
  color: var(--el-color-danger);
  font-size: 12.5px;
  padding: 4px 6px;
  white-space: pre-wrap;
}
.sw-item {
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 5px 8px;
  border-radius: 4px;
  cursor: pointer;
  font-size: 13px;
  line-height: 1.4;
}
.sw-item:hover {
  background: var(--el-fill-color-light);
}
.msg {
  white-space: pre-wrap;
}
</style>
