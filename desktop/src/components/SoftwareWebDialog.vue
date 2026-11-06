<script setup lang="ts">
// 打开软件的网页（Nacos 控制台、1Panel 面板……）：先选连法和在哪里打开。
// 用服务器地址直接打开（默认），或经服务器转发（这台电脑上开一个 127.0.0.1 的端口，经 SSH 转过去，服务器不用放行端口）；
// 在 Ferry 的内置浏览器里打开，或用系统的浏览器打开（经服务器转发时 Ferry 要开着）。
// 登录：列出能用的账号（记住的、Ferry 安装时保存的、环境变量里的、默认账号），选好后内置浏览器替用户填好、点登录；
// 系统的浏览器填不了，复制密码让用户粘贴。别的账号可以填上并记住（存在系统凭据库里）。
import { computed, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { CopyDocument, Delete } from '@element-plus/icons-vue'
import GlassToggle from './glass/GlassToggle.vue'
import { call, FerryError, type PortProbe, type Software } from '../ferry'
import { browserRequest, secretRequest, uiPref, setUiPref } from '../state'
import { copySecret, openUrl } from '../platform'
import { webEndpoint, webUrl } from '../softwareWeb'

const props = defineProps<{ sw: Software | null; config: string; target: string; host: string; title: string }>()
const emit = defineEmits<{ close: []; opened: [where: 'builtin' | 'system'] }>()

interface ForwardInfo { local: number; to: string; server: string }
interface WebAccount { user: string; password: string; from: string; saved?: boolean; default?: boolean; check?: 'ok' | 'bad' }
interface ReachResult { ips?: string[]; ok: boolean; why?: string; msg?: string }

const idx = ref(0)
const reach = ref<'direct' | 'ssh'>('direct')
const openIn = ref<'builtin' | 'system'>(uiPref('webOpenIn') === 'system' ? 'system' : 'builtin')
const busy = ref(false)
const error = ref('')
const web = computed(() => props.sw?.web ?? [])
const w = computed(() => web.value[idx.value])
const ep = computed(() => (props.sw && w.value ? webEndpoint(props.sw, w.value) : null))
const name = computed(() => props.sw?.instance || props.sw?.name || '')

// 按域名区分的网站（nginx 一个端口上有几个网站）：直接打开要用域名，用服务器 IP 打开看到的是默认站点
const directHost = computed(() => w.value?.host || props.host)
const directUrl = computed(() => (w.value && ep.value?.direct ? webUrl(w.value, directHost.value, ep.value.direct) : ''))
const sshTo = computed(() => {
  const e = ep.value
  if (!e) return ''
  return e.ssh.container ? `容器 ${e.ssh.container} 的 ${e.ssh.port} 端口` : `${e.ssh.host.includes(':') ? `[${e.ssh.host}]` : e.ssh.host}:${e.ssh.port}`
})
// 用户在对话框里打开过的转发：同一个网页再点「复制」不用再开
const forwarded = ref<{ key: string; url: string } | null>(null)
const fwdKey = computed(() => `${props.config}\n${props.target}\n${sshTo.value}\n${w.value?.host ?? ''}\n${w.value?.path ?? ''}`)

/** 经服务器转发时浏览器里用的名字：每个转发一个（nacos-38717.localhost），浏览器按名字分开存登录状态，
 *  几个网页都在 127.0.0.1 上时不会互相顶掉登录；xxx.localhost 浏览器都解析到本机 */
function localName(port: number): string {
  const slug = (props.sw?.kind || props.sw?.name || 'web').toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 20) || 'web'
  return `${slug}-${port}.localhost`
}

// 直接打开前先试这台电脑能不能连上（被防火墙、安全组挡住时直接说，建议经服务器转发）
const probe = ref<{ state: 'checking' | 'ok' | 'fail'; text?: string } | null>(null)
let probeSeq = 0
async function runProbe() {
  const seq = ++probeSeq
  probe.value = null
  const port = ep.value?.direct
  if (!props.sw || reach.value !== 'direct' || !port) return
  probe.value = { state: 'checking' }
  try {
    if (w.value?.host) {
      // 用域名打开：这台电脑要能解析这个域名
      const r = await call<ReachResult>(['forward', 'reach', w.value.host, String(port)])
      if (seq !== probeSeq) return
      const ips = r.ips?.length ? `（解析到 ${r.ips.slice(0, 3).join('、')}）` : ''
      probe.value = r.ok ? { state: 'ok', text: ips }
        : { state: 'fail', text: r.why === 'resolve' ? `这台电脑解析不了 ${w.value.host}（没有 DNS 记录，或只在内网能用）` : `${ips}${r.why === 'timeout' ? '没有回应' : r.why === 'refused' ? '被回绝了' : r.msg || '连不上'}` }
      return
    }
    const [r] = await call<PortProbe[]>(['forward', 'probe', props.target, String(port), '-c', props.config])
    if (seq !== probeSeq) return
    probe.value = r?.ok ? { state: 'ok' }
      : { state: 'fail', text: r?.why === 'timeout' ? '没有回应，多半被防火墙或安全组挡住了' : r?.why === 'refused' ? '服务器回绝了（防火墙拒绝，或没在这个端口监听）' : r?.msg || '连不上' }
  } catch {
    if (seq === probeSeq) probe.value = null
  }
}

// ---------------------------------------------------------------- 登录用的账号

const accounts = ref<WebAccount[] | null>(null)
const accountsLoading = ref(false)
/** 选的账号：accounts 的下标；'other' 填别的账号；'none' 不自动登录 */
const pick = ref<number | 'other' | 'none'>('none')
const other = ref({ user: '', password: '', remember: true })
let accSeq = 0

function webQuery() {
  const sw = props.sw!
  const x = w.value!
  return { kind: sw.kind ?? '', container: sw.via === 'docker' ? sw.container : undefined, pid: sw.pid, instance: sw.instance, account: sw.account,
    port: x.port || x.inner || 0, label: `${sw.name} ${x.label}`,
    // 试账号用：服务器上怎么连这个网页
    hostPort: x.port, innerPort: x.inner, scheme: x.scheme, path: x.path, vhost: x.host }
}

async function loadAccounts() {
  const seq = ++accSeq
  accounts.value = null
  pick.value = 'none'
  other.value = { user: /账号 ([^；，,\s]+)/.exec(w.value?.note ?? '')?.[1] ?? '', password: '', remember: true }
  if (!props.sw || !w.value) return
  accountsLoading.value = true
  try {
    const list = await call<WebAccount[]>(['forward', 'accounts', props.target, '-c', props.config], { FERRY_WEB_REQ: JSON.stringify(webQuery()) })
    if (seq !== accSeq) return
    accounts.value = list ?? []
    // 先选试过能登录的；没试过的里不选默认账号（多半改过，自动点登录会失败）；登录不上的不选
    const list2 = accounts.value
    const okAt = list2.findIndex((a) => a.check === 'ok')
    const fallback = list2.findIndex((a) => !a.default && a.check !== 'bad')
    pick.value = okAt >= 0 ? okAt : fallback >= 0 ? fallback : 'none'
  } catch {
    if (seq === accSeq) accounts.value = []
  } finally {
    if (seq === accSeq) accountsLoading.value = false
  }
}

async function forget(a: WebAccount) {
  try {
    await call(['forward', 'forget-account', props.target, '-c', props.config], { FERRY_WEB_REQ: JSON.stringify({ ...webQuery(), user: a.user }) })
    void loadAccounts()
  } catch (e) {
    error.value = (e as Error).message
  }
}

/** 选好的账号；填别的账号并勾了记住时先记住 */
async function chosenLogin(): Promise<{ user: string; password: string } | undefined> {
  if (pick.value === 'none') return undefined
  if (pick.value === 'other') {
    const user = other.value.user.trim()
    if (!user || !other.value.password) throw new Error('请填账号和密码，或选「不自动登录」')
    if (other.value.remember) {
      await call(['forward', 'save-account', props.target, '-c', props.config], { FERRY_WEB_REQ: JSON.stringify({ ...webQuery(), user }), FERRY_WEB_PASSWORD: other.value.password })
    }
    return { user, password: other.value.password }
  }
  const a = accounts.value?.[pick.value]
  return a ? { user: a.user, password: a.password } : undefined
}

watch(() => props.sw, (sw) => {
  if (!sw) return
  idx.value = 0
  error.value = ''
  forwarded.value = null
  pickReach()
  void loadAccounts()
}, { immediate: true })
watch(idx, () => {
  error.value = ''
  pickReach()
  void loadAccounts()
})
watch(reach, () => {
  error.value = ''
  void runProbe()
})
// 默认直接打开；直接连不上的（只在服务器本机监听、端口只在容器里）经服务器转发
function pickReach() {
  const r = ep.value?.direct ? 'direct' : 'ssh'
  if (reach.value === r) void runProbe()
  reach.value = r
}

/** 要打开的网址：经服务器转发时先开转发 */
async function resolveUrl(): Promise<string> {
  if (!w.value || !ep.value || !props.sw) return ''
  if (reach.value === 'direct') return directUrl.value
  if (forwarded.value?.key === fwdKey.value) return forwarded.value.url
  const e = ep.value
  const x = w.value
  const args = ['forward', props.target, String(e.ssh.port), '-c', props.config]
  if (e.ssh.container) args.push('--container', e.ssh.container)
  else if (e.ssh.host && e.ssh.host !== '127.0.0.1') args.push('--host', e.ssh.host)
  // 按域名区分的网站：转发时把请求里的 Host 换成域名（本机这边是 http，服务器那边按原来的 http / https）
  if (x.host) {
    args.push('--vhost', x.host)
    if (x.scheme === 'https') args.push('--tls')
  }
  const info = await call<ForwardInfo>(args)
  const url = webUrl(x.host ? { scheme: 'http', path: x.path } : x, localName(info.local), info.local)
  forwarded.value = { key: fwdKey.value, url }
  return url
}

async function withUrl(fn: (url: string) => Promise<void> | void) {
  busy.value = true
  error.value = ''
  try {
    const url = await resolveUrl()
    if (url) await fn(url)
  } catch (e) {
    const err = e as FerryError
    if (err instanceof FerryError && err.needSecret && err.detail) {
      secretRequest.value = { detail: err.detail, config: props.config, retry: () => void withUrl(fn) }
      return
    }
    error.value = err.message
  } finally {
    busy.value = false
  }
}

// 系统的浏览器里打开了、要用户自己登录：对话框留着，用户要粘贴时再点「复制密码」（不自动放进剪贴板）
const openedSystem = ref<{ user: string; password: string } | null>(null)
watch(() => props.sw, () => { openedSystem.value = null })

function open() {
  setUiPref('webOpenIn', openIn.value)
  void withUrl(async (url) => {
    const login = await chosenLogin()
    if (openIn.value === 'builtin') {
      browserRequest.value = { url, login }
      emit('opened', 'builtin')
      emit('close')
      return
    }
    await openUrl(url)
    emit('opened', 'system')
    if (login) openedSystem.value = login
    else emit('close')
  })
}

async function copyText(what: 'user' | 'password') {
  const l = openedSystem.value
  if (!l) return
  try {
    if (what === 'user') {
      await navigator.clipboard.writeText(l.user)
      ElMessage.success('已复制账号')
    } else {
      const safe = await copySecret(l.password)
      ElMessage.success(safe ? '已复制密码（不进剪贴板历史，30 秒后自动清掉）' : '已复制密码')
    }
  } catch (e) {
    error.value = `复制失败：${(e as Error).message}`
  }
}

function copy() {
  void withUrl(async (url) => {
    await navigator.clipboard.writeText(url)
    ElMessage.success(reach.value === 'ssh' ? '已复制（经服务器转发的地址，Ferry 开着时能用）' : '已复制')
  })
}
</script>

<template>
  <el-dialog :model-value="!!sw" :title="`打开 ${name} 的网页`" width="560px" append-to-body @close="emit('close')">
    <div v-if="openedSystem" class="opened">
      <p>已在系统的浏览器里打开。系统的浏览器没法自动填，在登录页填：</p>
      <div class="acc-line"><span>账号</span><b class="mono">{{ openedSystem.user }}</b><el-button size="small" @click="copyText('user')">复制账号</el-button></div>
      <div class="acc-line"><span>密码</span><b class="mono">••••••••</b><el-button size="small" type="primary" @click="copyText('password')">复制密码</el-button></div>
      <p class="note">密码复制后不进 Windows 的剪贴板历史，30 秒后还在剪贴板里会自动清掉。</p>
    </div>
    <el-form v-else-if="sw && w && ep" label-width="84px" @submit.prevent>
      <el-form-item v-if="web.length > 1" label="网页">
        <el-radio-group v-model="idx">
          <el-radio v-for="(x, i) in web" :key="i" :value="i">{{ x.label }}</el-radio>
        </el-radio-group>
      </el-form-item>
      <el-form-item label="怎么连">
        <el-radio-group v-model="reach" class="col">
          <el-tooltip :disabled="!!ep.direct" :content="ep.why" placement="top">
            <el-radio value="direct" :disabled="!ep.direct">用服务器地址直接打开</el-radio>
          </el-tooltip>
          <el-radio value="ssh">经服务器转发（不用放行端口）</el-radio>
        </el-radio-group>
        <div class="note">
          <template v-if="reach === 'direct'">
            <span class="mono">{{ directUrl }}</span>：要这台电脑能连上 {{ directHost }} 的 {{ ep.direct }} 端口。
            <template v-if="w.host">按域名区分的网站要用域名打开，用服务器 IP 只能看到默认站点。</template>
            <span v-if="probe?.state === 'checking'">正在试能不能连上…</span>
            <span v-else-if="probe?.state === 'ok'" class="ok">这台电脑能连上{{ probe.text }}。</span>
            <span v-else-if="probe?.state === 'fail'" class="bad">这台电脑连不上：{{ probe.text }}。<el-button link type="primary" @click="reach = 'ssh'">改成经服务器转发</el-button></span>
          </template>
          <template v-else>
            这台电脑上开一个端口（127.0.0.1），经 {{ title }} 的 SSH 连接转到服务器上的 {{ sshTo }}<template v-if="w.host">，请求按域名 {{ w.host }} 发过去</template>。Ferry 开着时能用，30 分钟没人用自动关掉；同一个网页下次还用同一个地址，不用重新登录。Ferry 重启后要从这里再打开一次。
            <template v-if="ep.why"><br />{{ ep.why }}。</template>
          </template>
        </div>
      </el-form-item>
      <el-form-item label="在哪打开">
        <el-radio-group v-model="openIn">
          <el-radio value="builtin">Ferry 的内置浏览器</el-radio>
          <el-radio value="system">系统的浏览器</el-radio>
        </el-radio-group>
      </el-form-item>
      <el-form-item label="登录">
        <div v-if="accountsLoading" class="note">正在找能用的账号…</div>
        <el-radio-group v-else v-model="pick" class="col accounts">
          <div v-for="(a, i) in accounts ?? []" :key="i" class="acc">
            <el-radio :value="i">
              <span class="mono">{{ a.user }}</span>
              <span v-if="a.check === 'ok'" class="tag ok">试过能登录</span>
              <span v-else-if="a.check === 'bad'" class="tag bad">登录不上</span>
              <span class="from" :class="{ warn: a.default }">{{ a.from }}</span>
            </el-radio>
            <el-tooltip v-if="a.saved" content="不再记住这个账号" placement="top" :show-after="400">
              <el-button link :icon="Delete" class="forget" @click.stop="forget(a)" />
            </el-tooltip>
          </div>
          <el-radio value="other">用别的账号</el-radio>
          <div v-if="pick === 'other'" class="other">
            <el-input v-model="other.user" placeholder="账号" />
            <el-input v-model="other.password" type="password" show-password placeholder="密码" autocomplete="new-password" />
            <GlassToggle v-model="other.remember">记住（存在系统凭据库里，下次直接选）</GlassToggle>
          </div>
          <el-radio value="none">不自动登录</el-radio>
        </el-radio-group>
        <div class="note">
          <template v-if="pick !== 'none' && openIn === 'builtin'">打开后替你填好账号密码、点登录（有验证码的要自己填）。</template>
          <template v-else-if="pick !== 'none'">系统的浏览器没法自动填：打开后可以复制账号、密码，在登录页粘贴。</template>
          <template v-if="accounts?.some((a) => a.check)"><br />「试过」是经服务器调了一次它的登录接口。</template>
          <template v-if="w.note"><br v-if="pick !== 'none'" />{{ w.note }}</template>
        </div>
      </el-form-item>
    </el-form>
    <el-alert v-if="error" type="error" :closable="false" :title="error" class="msg" />
    <template #footer>
      <template v-if="openedSystem">
        <el-button type="primary" @click="emit('close')">完成</el-button>
      </template>
      <template v-else>
        <el-button :icon="CopyDocument" :loading="busy" @click="copy">复制地址</el-button>
        <el-button @click="emit('close')">取消</el-button>
        <el-button type="primary" :loading="busy || accountsLoading" @click="open">打开</el-button>
      </template>
    </template>
  </el-dialog>
</template>

<style scoped>
.col {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
}
.note {
  width: 100%;
  margin-top: 2px;
  line-height: 1.55;
  font-size: 12.5px;
  color: var(--el-text-color-secondary);
  word-break: break-all;
}
.mono {
  font-family: var(--el-font-family-mono, ui-monospace, monospace);
}
.ok {
  color: var(--el-color-success);
}
.bad {
  color: var(--el-color-warning);
}
.msg {
  white-space: pre-wrap;
  margin-top: 8px;
}
.accounts {
  width: 100%;
}
.tag {
  margin-left: 6px;
  font-size: 12px;
}
.opened {
  line-height: 1.7;
}
.acc-line {
  display: flex;
  align-items: center;
  gap: 10px;
  margin: 6px 0;
}
.acc-line > span {
  width: 36px;
  color: var(--el-text-color-secondary);
}
.acc-line > b {
  min-width: 140px;
}
.acc {
  display: flex;
  align-items: center;
  gap: 4px;
}
.from {
  margin-left: 8px;
  font-size: 12px;
  color: var(--el-text-color-secondary);
}
.from.warn {
  color: var(--el-color-warning);
}
.forget {
  font-size: 13px;
}
.other {
  display: flex;
  flex-direction: column;
  gap: 6px;
  width: 100%;
  margin: 2px 0 6px 24px;
  max-width: 360px;
}
</style>
