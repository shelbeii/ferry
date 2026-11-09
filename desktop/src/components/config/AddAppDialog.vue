<script setup lang="ts">
import { computed, onBeforeUnmount, reactive, ref, watch } from 'vue'
import { Delete, FolderOpened, Plus, Refresh } from '@element-plus/icons-vue'
import { call, FerryError, type NginxSite } from '../../ferry'
import { pickDir, pickFile, readConfig } from '../../platform'
import { secretRequest, trustRequest, trustedTick } from '../../state'
import { appNames, setupError, type AppSetup, type AppType, type RuntimeProbe } from '../../editor/appSetup'
import { keysAt, parse, scalarAt } from '../../editor/configDoc'

const props = defineProps<{ text: string; config: string; target?: string }>()
const emit = defineEmits<{ close: []; add: [form: AppSetup, deploy: boolean] }>()
const visible = ref(true)
const doc = computed(() => parse(props.text))
const targets = computed(() => keysAt(doc.value, ['targets']))
const builds = computed(() => keysAt(doc.value, ['builds']))
const f = reactive<AppSetup>({
  name: '', type: 'java', path: '', targets: [targets.value.includes(props.target ?? '') ? props.target! : targets.value[0]].filter(Boolean),
  build: '', buildCommand: '', buildWorkdir: '', user: '', javaSource: 'ferry', javaMajor: 17, javaPaths: {},
  jvmArgs: '', args: '', exec: '', env: '', port: '', serve: 'installed', instances: {}, domains: '', spa: true, proxies: [], sites: {}, sitePort: '',
})
if (f.targets.some((t) => !scalarAt(doc.value, ['targets', t, 'runAs']))) f.user = 'ferry'
const error = ref('')
const proxyHint = 'http://127.0.0.1:8080/'
const types: { value: AppType; label: string }[] = [
  { value: 'java', label: 'Java（JAR）' }, { value: 'binary', label: '原生程序' }, { value: 'static', label: '前端静态站点' },
  { value: 'python', label: 'Python 服务' }, { value: 'node', label: 'Node.js 服务' }, { value: 'dotnet', label: '.NET 服务' },
]
const directory = computed(() => !['java', 'binary'].includes(f.type))
const needsUser = computed(() => f.type !== 'static' || f.serve === 'standalone')
const needsProbe = computed(() => f.type === 'static' && (f.serve === 'installed' || f.serve === 'site') || f.type === 'java' && f.javaSource === 'server')
// 选了已有站点：各服务器选的站点端口一样（且那个端口上只有它）时，部署后检查它的首页
function siteOf(t: string) { return currentProbe(t)?.result?.sites?.find((s) => s.dir === f.sites?.[t]) }
watch(() => f.targets.map((t) => f.sites?.[t] ?? '').join('|'), () => {
  const ports = f.targets.map((t) => { const s = siteOf(t); return s && !s.shared && s.ports?.length ? String(s.ports[0]) : '' })
  f.sitePort = ports.length && ports.every((p) => p && p === ports[0]) ? ports[0] : ''
})
const defaults: Partial<Record<AppType, string>> = {
  python: '${running}/.venv/bin/uvicorn\nmain:app\n--host\n0.0.0.0\n--port\n8000',
  node: 'node\n${running}/dist/main.js', dotnet: 'dotnet\n${running}/MyApp.dll',
}
watch(() => f.type, (type) => {
  f.exec = defaults[type] ?? ''
  f.port = type === 'static' ? nextWebPort() : ''
  error.value = ''
})
function nextWebPort() {
  const apps = doc.value.toJS()?.apps ?? []
  const used = new Set(apps.map((a: { static?: { port?: string } }) => Number(String(a?.static?.port ?? '').split(':').at(-1))))
  for (let port = 8081; port < 65536; port++) if (!used.has(port)) return String(port)
  return '8081'
}
async function pickArtifact() {
  const path = directory.value ? await pickDir('选择要上传的产物目录') : await pickFile('选择构建产物', '产物', f.type === 'java' ? ['jar'] : [])
  if (path) f.path = path
}

// 每台服务器分别检测；只发送连接部分，避免大型配置超过 Windows 环境变量上限。
interface ProbeState { key: string; loading: boolean; result?: RuntimeProbe; error: string }
const probes = reactive<Record<string, ProbeState>>({})
let disposed = false
let retryTrust: (() => void) | undefined
onBeforeUnmount(() => { disposed = true; retryTrust = undefined })
watch(trustedTick, () => { const retry = retryTrust; retryTrust = undefined; retry?.() })
watch(trustRequest, (v) => { if (!v) retryTrust = undefined })
function snapshot(target: string) {
  const value = doc.value.toJS()
  return JSON.stringify({ version: value.version, kind: value.kind, project: value.project, vars: value.vars, targets: { [target]: value.targets?.[target] } })
}
function siteLabel(s: NginxSite) {
  const parts = [s.server]
  if (s.ports?.length) parts.push(s.ports.join('、') + ' 端口')
  if (s.names?.length) parts.push(s.names.join(' '))
  return parts.join(' · ')
}
function currentProbe(target: string) {
  const p = probes[target]
  return p?.key === snapshot(target) ? p : undefined
}
async function probe(target: string) {
  const key = snapshot(target)
  if (probes[target]?.key === key && probes[target]?.loading) return
  const state: ProbeState = reactive({ key, loading: true, error: '' })
  probes[target] = state
  delete f.instances[target]
  delete f.javaPaths[target]
  try {
    const t = doc.value.toJS().targets[target]
    const result = await call<RuntimeProbe>(['probe', target, '-c', props.config, '--from-env'], {
      FERRY_CONFIG_CONTENT: key,
      FERRY_PROBE_VALUE: JSON.stringify({ root: t.root || `/opt/${scalarAt(doc.value, ['project'])}`, runAs: t.runAs || '', ferryDir: t.ferryDir || '' }),
    })
    if (disposed || probes[target] !== state || key !== snapshot(target)) return
    state.result = result
    if (result.nginxInstances?.length === 1) f.instances[target] = result.nginxInstances[0].name
    if (result.javas?.length === 1) f.javaPaths[target] = result.javas[0].path
  } catch (e) {
    if (disposed || probes[target] !== state || key !== snapshot(target)) return
    state.error = (e as Error).message
    if (e instanceof FerryError && e.unknownHost) {
      // 新增的服务器尚未保存时，主机核对窗口还不能从磁盘读到它。
      const saved = parse(await readConfig(props.config).catch(() => '')).toJS()?.targets?.[target]
      const draft = doc.value.toJS().targets[target]
      if (!disposed && saved?.host === draft.host && (saved?.port || 22) === (draft.port || 22)) {
        retryTrust = () => probe(target)
        trustRequest.value = { target, config: props.config }
      }
      state.error = '请先核对主机公钥；新添加的服务器需要先保存服务器配置，再检测运行环境'
    } else if (e instanceof FerryError && e.needSecret && e.detail) {
      secretRequest.value = { detail: e.detail, config: props.config, retry: () => { if (!disposed) void probe(target) } }
    }
  } finally { state.loading = false }
}
watch(() => [needsProbe.value, f.targets.map((t) => snapshot(t)).join('\n')], () => {
  if (needsProbe.value) for (const target of f.targets) if (!currentProbe(target)) void probe(target)
}, { immediate: true })

function submit(deploy: boolean) {
  error.value = setupError(doc.value, f)
  if (!error.value && needsProbe.value) {
    for (const t of f.targets) {
      const result = currentProbe(t)?.result
      if (!result) { error.value = `${t}：请先检测运行环境`; break }
      const failure = result.checks.find((check) => check.level === 'error')
      if (failure) { error.value = `${t}：${failure.text}`; break }
      if (f.type === 'static' && f.serve === 'site' && !result.sites?.some((s) => !s.disabled && s.dir === f.sites?.[t])) { error.value = `${t}：请选择检测到的已有站点`; break }
      if (f.type === 'static' && f.serve === 'installed' && !result.nginxInstances?.some((n) => n.name === f.instances[t])) { error.value = `${t}：请选择检测到的 Nginx 实例`; break }
      if (f.type === 'java' && !result.javas?.some((j) => j.path === f.javaPaths[t])) { error.value = `${t}：请选择检测到的 JDK`; break }
    }
  }
  if (error.value) return
  if (deploy && f.type === 'static' && f.serve === 'files') { error.value = '仅发布文件不会启动网站，请选择 Nginx 托管方式'; return }
  emit('add', f, deploy)
}
</script>

<template>
  <el-dialog v-model="visible" title="新增应用" width="760px" top="5vh" append-to-body :close-on-click-modal="false" @closed="emit('close')">
    <el-form label-width="112px" class="app-setup" @submit.prevent="submit(false)">
      <el-form-item label="应用名" required><el-input v-model="f.name" placeholder="如 api、web" /></el-form-item>
      <el-form-item label="应用类型"><el-select v-model="f.type"><el-option v-for="type in types" :key="type.value" :label="type.label" :value="type.value" /></el-select></el-form-item>
      <el-form-item label="产物路径" required><div class="full"><div class="inline"><el-input v-model="f.path" :placeholder="directory ? '前端 dist 或服务端发布目录' : 'JAR 或可执行文件'" /><el-button :icon="FolderOpened" @click="pickArtifact">选择{{ directory ? '目录' : '文件' }}</el-button></div><div class="muted small">相对路径以配置文件所在目录为准；部署时上传这里的{{ directory ? '目录内容' : '文件' }}。</div></div></el-form-item>
      <el-form-item label="目标服务器" required><div class="full"><el-select v-model="f.targets" multiple placeholder="选择要运行此应用的服务器"><el-option v-for="t in targets" :key="t" :value="t" :label="`${t}（${scalarAt(doc, ['targets', t, 'host']) ?? ''}）`" /></el-select><div class="muted small">应用会加入所选服务器的应用范围，保留已有应用的范围。</div></div></el-form-item>
      <el-form-item label="部署前构建"><div class="full"><el-select v-model="f.build" placeholder="不构建，直接上传现有产物"><el-option label="不构建，直接上传现有产物" value="" /><el-option v-for="build in builds" :key="build" :value="build" :label="build" /><el-option label="填写构建命令" value="@inline" /></el-select><template v-if="f.build === '@inline'"><el-input v-model="f.buildCommand" class="gap" placeholder="如 npm run build、mvn package -DskipTests" /><el-input v-model="f.buildWorkdir" class="gap" placeholder="构建目录，留空为配置文件所在目录" /></template></div></el-form-item>

      <template v-if="f.type === 'static'">
        <el-form-item label="运行方式"><el-select v-model="f.serve"><el-option label="使用已安装的 Nginx" value="installed" /><el-option label="由 Ferry 自动安装并独立运行 Nginx" value="standalone" /><el-option label="只换服务器上已有站点的文件（1Panel、宝塔、容器里的 OpenResty / Nginx）" value="site" /><el-option label="仅发布文件，手动接入网站" value="files" /></el-select></el-form-item>
        <el-alert v-if="f.serve === 'site'" type="info" :closable="false" class="gap site-tip" title="服务器上原来就有 nginx / OpenResty 在运行这个站点时用：部署只把每一版的文件换进它的网站目录，不改它的配置、不重启它。第一次部署前会备份原来的文件，可以「回到接管前」。" />
        <template v-if="f.serve !== 'files' && f.serve !== 'site'">
          <el-form-item label="网站端口" required><el-input v-model="f.port" placeholder="8081 或 127.0.0.1:8081" /></el-form-item>
          <el-form-item label="站点域名"><el-input v-model="f.domains" placeholder="多个域名用空格或逗号分开；留空为端口默认站点" /></el-form-item>
          <el-form-item label="前端路由"><el-checkbox v-model="f.spa">支持 history 路由，找不到的路径返回 index.html</el-checkbox></el-form-item>
          <el-form-item label="接口转发">
            <div class="full">
              <template v-for="(proxy, index) in f.proxies" :key="index">
                <div class="inline gap">
                  <el-input v-model="proxy.path" placeholder="/api/" class="proxy-path" />
                  <el-input v-model="proxy.to" :placeholder="proxyHint" />
                  <el-button :icon="Delete" aria-label="删除接口转发" @click="f.proxies.splice(index, 1)" />
                </div>
              </template>
              <el-button link type="primary" :icon="Plus" @click="f.proxies.push({ path: '', to: '' })">添加接口转发</el-button>
            </div>
          </el-form-item>
        </template>
        <el-alert v-else-if="f.serve === 'files'" type="info" :closable="false" title="此方式只上传文件，需要自行配置 Web 服务器后才能访问。" class="gap" />
      </template>
      <el-form-item v-if="needsUser" label="运行用户"><div class="full"><el-input v-model="f.user" placeholder="留空沿用各服务器的 runAs，如 ferry" /><div class="muted small">使用非 root 用户；不存在时部署会自动创建。</div></div></el-form-item>
      <template v-if="f.type === 'java'">
        <el-form-item label="Java 环境"><el-select v-model="f.javaSource"><el-option label="由 Ferry 自动提供 JDK" value="ferry" /><el-option label="使用服务器已安装的 JDK" value="server" /></el-select></el-form-item>
        <el-form-item v-if="f.javaSource === 'ferry'" label="JDK 版本"><el-select v-model="f.javaMajor"><el-option v-for="v in [8, 11, 17, 21, 25]" :key="v" :value="v" :label="`Java ${v}`" /></el-select></el-form-item>
        <el-form-item label="JVM 参数"><el-input v-model="f.jvmArgs" type="textarea" :rows="2" placeholder="每行一个，如 -Xmx512m" /></el-form-item>
      </template>

      <section v-if="needsProbe" class="runtime-list">
        <div v-for="t in f.targets" :key="t" class="runtime-target">
          <div class="inline"><b>{{ t }}</b><span class="muted small">{{ scalarAt(doc, ['targets', t, 'host']) }}</span><span class="spacer" /><el-button size="small" :icon="Refresh" :loading="currentProbe(t)?.loading" @click="probe(t)">检测运行环境</el-button></div>
          <div v-if="currentProbe(t)?.error" class="ev-error small gap">{{ currentProbe(t)?.error }}</div>
          <template v-if="currentProbe(t)?.result">
            <template v-if="f.type === 'static' && f.serve === 'site'">
              <el-select v-model="f.sites![t]" class="gap" placeholder="选择已有的站点">
                <el-option v-for="s in currentProbe(t)?.result?.sites ?? []" :key="s.root + s.conf" :value="s.dir || s.root" :disabled="!!s.disabled" :label="siteLabel(s)" />
              </el-select>
              <div v-if="!currentProbe(t)?.result?.sites?.length" class="ev-warn small gap">没有找到正在运行的 nginx / OpenResty 站点（要用 root 或免密 sudo 登录才能看）。</div>
              <div v-else-if="siteOf(t)" class="muted small gap">网站目录：<span class="mono">{{ siteOf(t)?.dir }}</span><template v-if="siteOf(t)?.dir !== siteOf(t)?.root">（容器里是 {{ siteOf(t)?.root }}）</template> · 配置 {{ siteOf(t)?.conf }}</div>
              <div v-for="s in (currentProbe(t)?.result?.sites ?? []).filter((x) => x.disabled)" :key="'d' + s.root + s.conf" class="muted small gap">{{ siteLabel(s) }}：{{ s.disabled }}</div>
            </template>
            <el-select v-else-if="f.type === 'static'" v-model="f.instances[t]" class="gap" placeholder="选择已安装的 Nginx">
              <el-option v-for="n in currentProbe(t)?.result?.nginxInstances ?? []" :key="n.name" :value="n.name" :label="`${n.hosting?.system ? '系统 Nginx' : n.name}（${n.unit}${n.version ? ' · ' + n.version : ''}）`" />
            </el-select>
            <el-select v-else v-model="f.javaPaths[t]" class="gap" placeholder="选择已安装的 JDK"><el-option v-for="j in currentProbe(t)?.result?.javas ?? []" :key="j.path" :value="j.path" :label="`Java ${j.major} · ${j.path}`" /></el-select>
            <div v-if="f.type === 'static' && f.serve === 'installed' && !currentProbe(t)?.result?.nginxInstances?.length" class="ev-warn small gap">未找到可托管站点的 Nginx。可先在「软件」中安装后重新检测，或选择由 Ferry 自动安装并独立运行。</div>
            <div v-if="f.type === 'java' && !currentProbe(t)?.result?.javas?.length" class="ev-warn small gap">没有检测到 JDK，可以改用由 Ferry 自动提供。</div>
            <div v-if="f.type === 'static' && f.serve === 'installed' && f.instances[t]" class="muted small gap">站点目录：{{ currentProbe(t)?.result?.nginxInstances?.find((n) => n.name === f.instances[t])?.hosting?.sites }}</div>
            <div v-for="(check, i) in currentProbe(t)?.result?.checks.filter((c) => c.level !== 'ok') ?? []" :key="i" :class="check.level === 'error' ? 'ev-error' : 'ev-warn'" class="small gap">{{ check.text }}</div>
          </template>
        </div>
      </section>
      <template v-if="f.type !== 'static'">
        <el-form-item v-if="['python', 'node', 'dotnet'].includes(f.type)" label="启动命令" required><div class="full"><el-input v-model="f.exec" type="textarea" :rows="5" placeholder="每行一个参数，第一行为程序" /><div class="muted small">请按实际入口修改；${running} 为服务器上的运行目录。Python、Node.js 默认在部署时安装目录中的依赖。</div></div></el-form-item>
        <el-form-item v-else label="程序参数"><el-input v-model="f.args" type="textarea" :rows="2" placeholder="每行一个参数，如 --spring.profiles.active=prod" /></el-form-item>
        <el-form-item label="环境变量"><el-input v-model="f.env" type="textarea" :rows="2" placeholder="每行 KEY=值；密码等请在应用的 envFile 中引用" /></el-form-item>
        <el-form-item label="健康检查端口"><div class="full"><el-input v-model="f.port" placeholder="可选，如 8080" /><div class="muted small">用于确认启动成功，请与程序实际监听端口一致。</div></div></el-form-item>
      </template>
    </el-form>
    <p class="muted small">{{ appNames(doc).length ? '添加后可继续修改高级配置。' : '添加第一个应用后即可保存项目配置。' }}部署会自动上传产物、生成配置并启动服务。</p>
    <el-alert v-if="error" :title="error" type="error" :closable="false" show-icon />
    <template #footer><el-button @click="visible = false">取消</el-button><el-button @click="submit(false)">添加</el-button><el-button v-if="target" type="primary" :disabled="f.type === 'static' && f.serve === 'files'" @click="submit(true)">添加并部署</el-button></template>
  </el-dialog>
</template>

<style scoped>
.app-setup { max-height: calc(84dvh - 180px); overflow-y: auto; padding-right: 12px; }
.full { width: 100%; }
.inline { display: flex; align-items: center; gap: 8px; width: 100%; }
.inline > .el-input { flex: 1; min-width: 0; }
.inline > .proxy-path { flex: 0 0 110px; }
.gap { margin-top: 8px; }
.spacer { flex: 1; }
.runtime-list { border: 1px solid var(--el-border-color); border-radius: 8px; padding: 12px; margin: 0 0 18px 112px; }
.runtime-target + .runtime-target { margin-top: 16px; padding-top: 12px; border-top: 1px solid var(--el-border-color-lighter); }
.runtime-target .small { line-height: 1.6; overflow-wrap: anywhere; }
</style>
