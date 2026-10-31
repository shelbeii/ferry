<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { ArrowLeft, Search, Plus, Upload } from '@element-plus/icons-vue'
import { stringify } from 'yaml'
import { call, variantMisfit, type Software, type SoftwareReport, type SwPackage, type SwParam, type SwTemplate, type SwTemplates, type SwVariant } from '../ferry'
import { startTask, type Task } from '../state'
import { pickFile } from '../platform'
import LoadingBlock from './LoadingBlock.vue'

const props = defineProps<{ config: string; target: string; report?: SoftwareReport; done: (t: Task) => void }>()
const emit = defineEmits<{ close: [] }>()

// ---------------------------------------------------------------- 软件池

const info = ref<SwTemplates>()
const loadError = ref('')
const query = ref('')
const category = ref('全部')
const view = ref<'store' | 'template' | 'custom'>('store')

async function load() {
  try {
    info.value = await call<SwTemplates>(['software', 'templates'])
    loadError.value = ''
  } catch (e) {
    loadError.value = (e as Error).message
  }
}
onMounted(load)

const templates = computed(() => info.value?.templates ?? [])
const library = computed<SwPackage[]>(() => info.value?.library ?? [])
const categories = computed(() => ['全部', ...new Set(templates.value.map((t) => t.category || '其他'))])

const os = computed(() => props.report?.os)
const arch = computed(() => props.report?.arch)
const osName = computed(() => os.value?.pretty || os.value?.id || '')

/** 适合这台服务器的第一种装法；没有时为 -1 */
function bestVariant(t: SwTemplate): number {
  return t.variants.findIndex((v) => !variantMisfit(v, os.value, arch.value))
}

const shown = computed(() => {
  const q = query.value.trim().toLowerCase()
  return templates.value.filter(
    (t) =>
      (category.value === '全部' || (t.category || '其他') === category.value) &&
      (!q || [t.id, t.name, t.desc, t.category].some((x) => x?.toLowerCase().includes(q))),
  )
})

function howTo(v: SwVariant): string {
  // 有安装包时 system 只是它需要的系统库
  if (v.package?.url) return '下载官方安装包'
  if (v.package) return '导入安装包'
  return '系统软件源'
}

// ---------------------------------------------------------------- 按模板安装

const tpl = ref<SwTemplate>()

// AI 写的模板：把要执行的内容列出来给用户看
const aiSteps = computed(() => {
  const v = tpl.value?.variants[vi.value]
  if (!v) return ''
  const lines: string[] = []
  if (v.system?.dnf?.length || v.system?.apt?.length) lines.push(`装软件包：${[...new Set([...(v.system.dnf ?? []), ...(v.system.apt ?? [])])].join(' ')}`)
  for (const f of v.files ?? []) lines.push(`写文件：${f.path}`)
  for (const s of v.steps ?? []) lines.push(s.run ? `执行：${s.run}` : `请求：${s.http?.post}`)
  if (v.service?.exec) lines.push(`启动：${v.service.exec}`)
  return lines.join('\n')
})
const vi = ref(0)
const variant = computed(() => tpl.value?.variants[vi.value])
const params = computed<SwParam[]>(() => tpl.value?.params ?? [])
const form = ref({ name: '', in: '/opt/ferry-software', user: '', version: '', java: 0, pkg: '', javaDir: '', network: 'auto' as Network })
// 服务器要下载东西（从软件源装包、步骤里的下载）时走谁的网络
type Network = 'auto' | 'local' | 'server'
const downloads = computed(() => !!variant.value?.system || !!variant.value?.steps?.some((st) => st.run))
const values = ref<Record<string, string | number | boolean>>({})
const latest = ref('')
const latestError = ref('')
const latestLoading = ref(false)

const taken = computed(() => new Set((props.report?.items ?? []).map((i) => i.instance).filter(Boolean)))

function freeName(base: string) {
  let n = base
  for (let i = 2; taken.value.has(n); i++) n = `${base}-${i}`
  return n
}

function open(t: SwTemplate) {
  tpl.value = t
  const best = bestVariant(t)
  vi.value = best >= 0 ? best : 0
  const name = freeName(t.id)
  form.value = { name, in: form.value.in || '/opt/ferry-software', user: '', version: '', java: 0, pkg: '', javaDir: '', network: form.value.network || 'auto' }
  values.value = {}
  for (const p of params.value) {
    if (p.type === 'bool') values.value[p.key] = p.default === 'true'
    else if (p.type === 'port' || p.type === 'number') values.value[p.key] = Number(p.default ?? 0)
    else values.value[p.key] = p.default ?? ''
  }
  // 默认的端口被占着：换成后面空闲的（附加端口一起避开），说明原因
  hints.value = {}
  for (const p of params.value) {
    if (p.type !== 'port') continue
    const why = paramError(p)
    if (!why) continue
    const q = nextFree(p)
    if (q) {
      hints.value[p.key] = { value: q, text: `默认的 ${values.value[p.key]} ${why.replace(/^\d+(（\+\d+）)? ?/, '')}，已换成 ${q}` }
      values.value[p.key] = q
    }
  }
  // 默认内存超过服务器可用内存的一半：调小
  const avail = props.report?.memAvailMB ?? 0
  for (const p of params.value) {
    if (p.type !== 'memory' || !avail) continue
    const v = String(values.value[p.key])
    if (memMB(v) * 2 <= avail) continue
    const fit = [...memOptions].reverse().find((m) => memMB(m) * 2 <= avail) ?? memOptions[0]
    if (fit !== v) {
      values.value[p.key] = fit
      hints.value[p.key] = { value: fit, text: `服务器现在可用内存约 ${gb(avail)}，默认的 ${v} 偏大，已改成 ${fit}` }
    }
  }
  applyVariant()
  view.value = 'template'
}

function applyVariant() {
  const v = variant.value
  if (!v) return
  form.value.user = v.user
  form.value.java = v.java ?? 0
  form.value.version = ''
  form.value.pkg = ''
  latest.value = ''
  latestError.value = ''
  if (v.package?.url) loadLatest()
  else if (v.package) form.value.pkg = library.value.find((m) => m.template === tpl.value?.id)?.file ?? ''
}
watch(vi, applyVariant)

async function loadLatest() {
  const t = tpl.value
  if (!t) return
  latestLoading.value = true
  try {
    const r = await call<{ version: string }>(['software', 'latest', t.id, '--variant', String(vi.value + 1)])
    if (tpl.value === t) latest.value = r.version
  } catch (e) {
    latestError.value = (e as Error).message
  } finally {
    latestLoading.value = false
  }
}

const localPkg = computed(() => {
  const t = tpl.value
  const ver = form.value.version || latest.value || variant.value?.package?.fallback
  return library.value.find((m) => m.template === t?.id && (!ver || m.version === ver))
})

// 要装的版本（安装目录的文件夹名用）：填的、选的安装包的、查到的最新版本；查不到最新时和 ferry 一样退回本机已有的或备用版本
const shownVersion = computed(() => {
  if (view.value === 'custom') return library.value.find((m) => m.file === custom.value.pkg)?.version ?? ''
  const p = variant.value?.package
  if (!p) return ''
  if (form.value.version) return form.value.version
  if (form.value.pkg) return library.value.find((m) => m.file === form.value.pkg)?.version ?? ''
  if (latest.value) return latest.value
  if (latestError.value) return library.value.find((m) => m.template === tpl.value?.id)?.version || p.fallback || ''
  return ''
})
// 安装目录：安装位置下的「名称-版本」文件夹（和 ferry 的 SoftwareFolder 一样）
const folder = (name: string, version: string) => {
  const v = version.replace(/[^A-Za-z0-9._-]+/g, '_').replace(/^[._-]+|[._-]+$/g, '')
  return !v || name.endsWith('-' + v) ? name : `${name}-${v}`
}
const installName = computed(() => (view.value === 'custom' ? custom.value.name : form.value.name))
const installDir = computed(() => {
  const base = form.value.in.trim().replace(/\/+$/, '') || '/opt/ferry-software'
  const waiting = view.value !== 'custom' && !shownVersion.value && latestLoading.value
  return `${base}/${waiting ? `${installName.value}-（最新版本）` : folder(installName.value, shownVersion.value)}`
})
const inError = computed(() => {
  const d = form.value.in.trim().replace(/\/+$/, '')
  return /^\/[A-Za-z0-9._-]+(\/[A-Za-z0-9._-]+)*$/.test(d) ? '' : '写绝对路径，只能用字母、数字、点、下划线、横线'
})

// 安装目录所在分区：最长的匹配前缀
const disk = computed(() => {
  const ds = props.report?.disks ?? []
  const dir = installDir.value
  return ds.filter((d) => dir === d.path || dir.startsWith(d.path + '/')).sort((a, b) => b.path.length - a.path.length)[0]
})
const roomier = computed(() => {
  const cur = disk.value?.freeMB ?? -1
  return (props.report?.disks ?? []).filter((d) => d.freeMB > cur * 2 && d.freeMB > 5 * 1024).sort((a, b) => b.freeMB - a.freeMB)[0]
})
const gb = (mb: number) => (mb >= 1024 ? `${(mb / 1024).toFixed(1)} GB` : `${mb} MB`)
const jdkReady = (n: number) => !!props.report?.runtimes?.[String(n)]
// JDK 安装目录：留空 = Ferry 的公共目录（多个软件、应用共用）
const sharedJdk = (n: number) => `${props.report?.ferryDir || '/usr/local/lib/ferry'}/runtimes/java-${n}`
const javaDirError = computed(() => {
  const d = form.value.javaDir.trim().replace(/\/+$/, '')
  if (!d) return ''
  if (!/^\/[A-Za-z0-9._-]+(\/[A-Za-z0-9._-]+)+$/.test(d)) return '写绝对路径，至少两级，只能用字母、数字、点、下划线、横线'
  const dir = installDir.value
  if (d === dir || d.startsWith(dir + '/') || dir.startsWith(d + '/')) return '不能和安装目录重叠'
  return ''
})
function javaDirHint(n: number): string {
  const d = form.value.javaDir.trim().replace(/\/+$/, '')
  if (d) return `JDK 装在这个目录下以版本命名的文件夹里（如 ${d}/jdk-${n}.0.x-x64），里面已有 Ferry 装的 JDK ${n} 时直接用`
  return jdkReady(n) ? `服务器上已有 ${sharedJdk(n)}，直接用` : `装到 ${sharedJdk(n)}，以后别的软件、应用也能用`
}

const used = computed(() => new Set(props.report?.ports ?? []))
const usedUDP = computed(() => new Set(props.report?.udpPorts ?? []))
const protos = (p: SwParam) => (p.proto === 'udp' ? ['udp'] : p.proto === 'both' ? ['tcp', 'udp'] : ['tcp'])
/** 端口参数占用的全部端口（含附加端口），按协议 */
const portsOf = (p: SwParam) => [0, ...(p.offsets ?? [])].map((off) => Number(values.value[p.key]) + off)
function portTaken(pr: string, n: number): string {
  if (!(pr === 'udp' ? usedUDP : used).value.has(n)) return ''
  const who = portOwner(pr, n)
  return who ? `已被 ${who}占用` : '已被占用'
}
/** 端口被谁占着：Ferry 装的软件看 portUsers，其他的看软件列表里谁在监听它 */
function portOwner(pr: string, n: number): string {
  const who = props.report?.portUsers?.[`${pr}/${n}`]
  if (who || pr !== 'tcp') return who ?? ''
  const it = (props.report?.items ?? []).find((i) => (i.listen ?? []).some((l) => l.endsWith(`:${n}`)))
  return it ? `${it.name}${it.version ? ' ' + it.version : ''}（${it.unit || (it.container ? '容器 ' + it.container : '进程')}）` : ''
}
/** 端口参数 p 用 n 时它要占的端口都空着，也不和别的端口参数重复 */
function portOk(p: SwParam, n: number): boolean {
  for (const pr of protos(p)) {
    const others = params.value.filter((q) => q.type === 'port' && q.key !== p.key && protos(q).includes(pr)).flatMap(portsOf)
    for (const off of [0, ...(p.offsets ?? [])]) {
      const q = n + off
      if (q < 1 || q > 65535 || (pr === 'udp' ? usedUDP : used).value.has(q) || others.includes(q)) return false
    }
  }
  return true
}
/** 往后找一个能用的端口；找不到为 0 */
function nextFree(p: SwParam): number {
  const n = Number(values.value[p.key])
  for (let q = n + 1; q <= Math.min(65535, n + 1000); q++) if (portOk(p, q)) return q
  return 0
}
// 打开表单时替用户改了默认值的原因（端口被占、内存太大）：用户再改了就不显示
const hints = ref<Record<string, { value: string | number; text: string }>>({})
const memOptions = ['256m', '512m', '1g', '2g', '4g']
const memMB = (s: string) => {
  const m = /^(\d+)([mg])$/i.exec(s.trim())
  return m ? Number(m[1]) * (m[2].toLowerCase() === 'g' ? 1024 : 1) : 0
}
/** 内存设得比服务器现在可用的还大：提醒（不拦着） */
function memWarn(p: SwParam): string {
  const avail = props.report?.memAvailMB ?? 0
  const mb = memMB(String(values.value[p.key]))
  return avail && mb > avail ? `服务器现在可用内存只有约 ${gb(avail)}，设得比这还大可能起不来或被系统杀掉` : ''
}

// 服务器上已经有同类软件：提醒可以直接用它
const kindFamily: Record<string, string[]> = { mysql: ['mysql', 'mariadb'], mariadb: ['mysql', 'mariadb'], redis: ['redis', 'valkey'] }
const sameKind = computed(() => {
  const t = tpl.value
  if (!t) return []
  const kinds = kindFamily[t.id] ?? [t.id]
  return (props.report?.items ?? []).filter((i) => i.kind && kinds.includes(i.kind))
})
const viaLabel = (i: Software) => (i.instance ? 'Ferry 装的' : i.via === 'docker' ? '容器' : i.via === 'systemd' ? '系统服务' : '进程')

// 安装时会做的事：装之前看清楚
const summary = computed<string[]>(() => {
  const v = variant.value
  if (!v) return []
  const out: string[] = []
  const sys = pkgMgrApt.value ? v.system?.apt : v.system?.dnf
  if (sys?.length) {
    const names = sys.map((x) => x.split('|').join(' 或 ')).join('、')
    out.push(`${v.package ? '需要的系统库' : '程序'}从服务器的软件源装：${names}（已经装着的直接用、不升级；会顺带升级或删除别的已装包时先问你）`)
  }
  if (v.package?.url && !form.value.pkg) {
    out.push(localPkg.value ? `上传本机已有的 ${localPkg.value.file}` : `下载 ${tpl.value?.name} ${shownVersion.value || ''}（${v.package.size || '大小未知'}）到这台电脑，再上传到服务器`)
  } else if (v.package) {
    out.push(form.value.pkg ? `上传本机库里的 ${form.value.pkg}` : '上传选的安装包')
  }
  if (v.java && (form.value.javaDir.trim() || !jdkReady(form.value.java))) out.push(`装 JDK ${form.value.java}（本机没有时先下载，约 190 MB）`)
  out.push(`运行用户 ${form.value.user}（没有会创建不能登录的系统用户）`)
  out.push(`装到 ${installDir.value}，建系统服务 ferry-${form.value.name} 并开机自动启动`)
  const tcp = params.value.filter((p) => p.type === 'port' && protos(p).includes('tcp')).flatMap(portsOf)
  const udp = params.value.filter((p) => p.type === 'port' && protos(p).includes('udp')).flatMap(portsOf)
  if (tcp.length || udp.length) out.push(`占用端口 ${[...tcp, ...udp.map((x) => `${x}/UDP`)].join('、')}`)
  if (v.selinux) out.push('服务器开着 SELinux 时，给安装目录和端口加上 SELinux 标签（程序才能读写、监听）')
  out.push('启动后等端口能连上；有密码的软件再用设置的账号和密码登录一次，登录不上就撤销安装')
  return out
})
const pkgMgrApt = computed(() => props.report?.pkgMgr === 'apt-get')
function paramError(p: SwParam): string {
  const v = values.value[p.key]
  switch (p.type) {
    case 'port': {
      const n = Number(v)
      if (!n || n < 1 || n > 65535) return '端口要在 1～65535 之间'
      for (const pr of protos(p)) {
        const tag = pr === 'udp' && p.proto === 'both' ? ' UDP' : ''
        for (const off of [0, ...(p.offsets ?? [])]) {
          const taken = portTaken(pr, n + off)
          if (taken) return off ? `${n + off}${tag}（+${off}）${taken}` : `${n}${tag} ${taken}`
        }
        const others = params.value.filter((q) => q.type === 'port' && q.key !== p.key && protos(q).includes(pr)).flatMap(portsOf)
        if (portsOf(p).some((x) => others.includes(x))) return '和其他端口重复'
      }
      return ''
    }
    case 'password':
      return v && !/^[A-Za-z0-9!@#%^*()_+=.,:~-]{8,64}$/.test(String(v)) ? '8～64 位，只能用字母、数字和 !@#%^*()_+=.,:~-' : ''
    case 'memory':
      return /^[1-9][0-9]{1,5}[mg]$/i.test(String(v)) ? '' : '写成 512m、2g 这样'
    case 'account':
      return /^[A-Za-z][A-Za-z0-9_]{2,31}$/.test(String(v)) ? '' : '3～32 位，字母开头，只能用字母、数字、下划线'
  }
  return ''
}
const nameError = computed(() =>
  !/^[a-z][a-z0-9-]{0,30}$/.test(form.value.name) ? '小写字母、数字、横线，以字母开头' : taken.value.has(form.value.name) ? '已经有同名的了' : '',
)
const misfit = computed(() => (variant.value ? variantMisfit(variant.value, os.value, arch.value) : ''))
const needPkg = computed(() => !!variant.value?.package && !variant.value.package.url && !form.value.pkg)
const canInstall = computed(
  () => tpl.value && !misfit.value && !nameError.value && !inError.value && !needPkg.value && !javaDirError.value && params.value.every((p) => !paramError(p)),
)

async function importPkg(after?: (m: SwPackage) => void) {
  const f = await pickFile('选择安装包（官方的文件名不要改；自己的程序、压缩包都可以）', '安装包', ['*'])
  if (!f) return
  try {
    const m = await call<SwPackage>(['software', 'lib', 'import', f])
    ElMessage.success(m.template ? `已导入 ${m.template} ${m.version}` : `已导入 ${m.file}`)
    await load()
    after?.(m)
  } catch (e) {
    ElMessage.error((e as Error).message)
  }
}

function paramsJSON(): string {
  const out: Record<string, string> = {}
  for (const p of params.value) {
    const v = values.value[p.key]
    if (p.type === 'password' && !v) continue // 不填 = 随机生成
    out[p.key] = String(v)
  }
  return JSON.stringify(out)
}

function install() {
  const t = tpl.value
  if (!t) return
  const f = form.value
  const args = ['software', 'install', props.target, t.id, '--name', f.name, '--in', f.in.trim().replace(/\/+$/, ''), '--user', f.user, '--variant', String(vi.value + 1)]
  // 版本和界面上显示的文件夹名一致
  const ver = f.version || (variant.value?.package?.url ? shownVersion.value : '')
  if (ver) args.push('--version', ver)
  if (f.pkg) args.push('--package', f.pkg)
  if (variant.value?.java && f.java && f.java !== variant.value.java) args.push('--java', String(f.java))
  if (variant.value?.java && f.javaDir.trim()) args.push('--java-dir', f.javaDir.trim().replace(/\/+$/, ''))
  if (downloads.value && f.network !== 'auto') args.push('--network', f.network)
  args.push('-c', props.config)
  startTask(`在 ${props.target} 上安装 ${t.name}（${f.name}）`, args, props.done, { FERRY_SOFTWARE_PARAMS: paramsJSON() })
  emit('close')
}

// ---------------------------------------------------------------- 自定义安装

const custom = ref({ pkg: '', title: '', name: '', exec: '', port: 8080, user: '', java: 0, format: '', strip: 1, into: '', save: false, id: '' })
const customFile = computed(() => library.value.find((m) => m.file === custom.value.pkg))
const extFormat = (file: string) => {
  const f = file.toLowerCase()
  if (f.endsWith('.tar.gz') || f.endsWith('.tgz')) return 'tar.gz'
  if (f.endsWith('.tar.xz') || f.endsWith('.txz')) return 'tar.xz'
  if (f.endsWith('.zip')) return 'zip'
  if (f.endsWith('.jar') || f.endsWith('.war')) return 'jar'
  return 'binary'
}
const isArchive = computed(() => ['tar.gz', 'tar.xz', 'zip'].includes(custom.value.format))

function openCustom() {
  custom.value = { pkg: '', title: '', name: freeName('app'), exec: '', port: 8080, user: '', java: 0, format: '', strip: 1, into: '', save: false, id: '' }
  tried.value = false
  form.value.javaDir = ''
  view.value = 'custom'
}
watch(
  () => custom.value.pkg,
  (file) => {
    if (!file) return
    const fmt = extFormat(file)
    custom.value.format = fmt
    custom.value.into = fmt === 'jar' || fmt === 'binary' ? file : ''
    const base = file.replace(/(\.tar\.gz|\.tgz|\.tar\.xz|\.zip|\.jar|\.war)$/i, '').replace(/[-_.]?v?\d.*$/, '')
    if (!custom.value.title) custom.value.title = base || file
    if (!custom.value.exec) {
      custom.value.exec = fmt === 'jar' ? `\${java} -jar \${dir}/${file} --server.port=\${port}` : fmt === 'binary' ? `\${dir}/${file}` : '${dir}/bin/'
      if (fmt === 'jar' && !custom.value.java) custom.value.java = 17
    }
  },
)

// 点了「安装」之后才把没填好的标红
const tried = ref(false)
const shownErrors = computed<Record<string, string>>(() => (tried.value ? customErrors.value : {}))

const customErrors = computed(() => {
  const c = custom.value
  const e: Record<string, string> = {}
  if (!c.pkg) e.pkg = '选一个安装包（没有就先导入）'
  if (!/^[a-z][a-z0-9-]{0,30}$/.test(c.name)) e.name = '小写字母、数字、横线，以字母开头'
  else if (taken.value.has(c.name)) e.name = '已经有同名的了'
  if (!c.exec.trim()) e.exec = '写启动命令'
  else if (c.exec.includes('${java}') && !c.java) e.exec = '用了 ${java}：要选 JDK'
  if (c.java && javaDirError.value) e.javaDir = javaDirError.value
  if (inError.value) e.in = inError.value
  if (!c.port || c.port < 1 || c.port > 65535) e.port = '端口要在 1～65535 之间'
  else if (portTaken('tcp', c.port)) e.port = `${c.port} ${portTaken('tcp', c.port)}`
  if (c.save && !/^[a-z][a-z0-9-]{0,30}$/.test(c.id)) e.id = '模板 ID：小写字母、数字、横线，以字母开头'
  return e
})

/** 自定义安装生成的模板 */
function customYAML(): string {
  const c = custom.value
  const id = c.save ? c.id : c.name
  const pkg: Record<string, unknown> = { format: c.format }
  if (isArchive.value && c.strip) pkg.strip = c.strip
  if (c.into) pkg.into = c.into
  const v: Record<string, unknown> = { label: '导入的安装包', user: c.user || id.replace(/-/g, '_'), package: pkg }
  if (c.java) v.java = c.java
  v.service = { exec: c.exec.trim().replace(/\s*\n\s*/g, ' '), workdir: '${dir}', ready: { port: '${port}' } }
  return stringify({
    id,
    name: c.title || id,
    category: '自定义',
    desc: `自定义安装：${c.pkg}`,
    params: [{ key: 'port', label: '端口', type: 'port', default: String(c.port) }],
    variants: [v],
  })
}

async function installCustom() {
  tried.value = true
  if (Object.keys(customErrors.value).length > 0) return
  const c = custom.value
  const yaml = customYAML()
  let tplArg = '-'
  if (c.save) {
    try {
      await call(['software', 'template', 'save'], { FERRY_SOFTWARE_TEMPLATE: yaml })
      ElMessage.success(`已保存为模板 ${c.id}，以后在软件池里能找到`)
      tplArg = c.id
    } catch (e) {
      ElMessage.error((e as Error).message)
      return
    }
  }
  const args = ['software', 'install', props.target, tplArg, '--name', c.name, '--in', form.value.in.trim().replace(/\/+$/, ''), '--package', c.pkg, '-c', props.config]
  if (c.java && form.value.javaDir.trim()) args.push('--java-dir', form.value.javaDir.trim().replace(/\/+$/, ''))
  startTask(`在 ${props.target} 上安装 ${c.title || c.name}（${c.name}）`, args, props.done, tplArg === '-' ? { FERRY_SOFTWARE_TEMPLATE: yaml } : {})
  emit('close')
}
</script>

<template>
  <!-- 弹层挂到 body，避免被项目列表的液态玻璃遮罩裁剪。 -->
  <el-dialog :model-value="true" append-to-body width="920px" top="4vh" class="fit-dialog" @close="emit('close')">
    <template #header>
      <div class="head">
        <el-button v-if="view !== 'store'" :icon="ArrowLeft" link @click="view = 'store'">软件池</el-button>
        <span class="title">
          {{ view === 'store' ? `在 ${target} 上安装软件` : view === 'custom' ? '自定义安装' : tpl?.name }}
        </span>
        <span v-if="osName" class="muted small">这台服务器：{{ osName }}（{{ arch }}）</span>
      </div>
    </template>

    <el-alert v-if="loadError" type="error" :title="loadError" :closable="false" show-icon />
    <LoadingBlock v-else-if="!info" text="正在读取软件池…" :rows="6" min-height="300px" />

    <!-- 软件池 -->
    <template v-else-if="view === 'store'">
      <div class="bar">
        <el-radio-group v-model="category" size="small">
          <el-radio-button v-for="c in categories" :key="c" :value="c">{{ c }}</el-radio-button>
        </el-radio-group>
        <span class="spacer" />
        <el-input v-model="query" :prefix-icon="Search" placeholder="搜索" clearable size="small" class="search" />
      </div>
      <el-alert v-for="e in info.errors ?? []" :key="e" type="warning" :title="`模板有问题：${e}`" :closable="false" show-icon class="tpl-err" />
      <div class="grid">
        <div class="card custom" @click="openCustom">
          <div class="card-head">
            <el-icon><Plus /></el-icon><b>自定义安装</b>
          </div>
          <div class="muted small">选一个自己的程序或压缩包，填启动命令和端口，装成系统服务；可以存成模板以后再用</div>
        </div>
        <div v-for="t in shown" :key="t.id" class="card" :class="{ off: bestVariant(t) < 0 }" @click="open(t)">
          <div class="card-head">
            <b>{{ t.name }}</b>
            <el-tag size="small" type="info">{{ t.category || '其他' }}</el-tag>
            <el-tag v-if="t.origin === 'ai'" size="small" type="warning">AI 写的</el-tag>
            <el-tag v-else-if="!t.builtin" size="small" :type="t.overrides ? 'warning' : 'success'">{{ t.overrides ? '自己改过' : '自己的' }}</el-tag>
          </div>
          <div class="desc muted small">{{ t.desc }}</div>
          <div class="fit small">
            <template v-if="bestVariant(t) >= 0">
              <span class="ok">可以装</span>：{{ t.variants[bestVariant(t)].label }}（{{ howTo(t.variants[bestVariant(t)]) }}）
            </template>
            <span v-else class="muted">这台服务器用不了：{{ variantMisfit(t.variants[0], os, arch) }}</span>
          </div>
        </div>
      </div>
      <p class="muted small foot">模板在「设置 → 运行环境 → 软件模板」里管理。</p>
    </template>

    <!-- 按模板安装 -->
    <template v-else-if="view === 'template' && tpl && variant">
      <p class="muted small desc-line">
        {{ tpl.desc }}
        <template v-if="tpl.homepage">（{{ tpl.homepage }}）</template>
        作为系统服务 <span class="mono">ferry-{{ form.name }}</span> 运行，开机自动启动。
      </p>
      <el-alert v-if="tpl.origin === 'ai'" type="warning" :closable="false" show-icon class="ai-tpl">
        <template #title>这个模板是 AI 助手写的：安装时下面这些会以 root 执行，先看一下</template>
        <pre class="mono ai-steps">{{ aiSteps }}</pre>
      </el-alert>
      <el-alert v-if="sameKind.length" type="info" :closable="false" show-icon class="ai-tpl">
        <template #title>
          这台服务器上已经有：{{ sameKind.map((i) => `${i.name}${i.version ? ' ' + i.version : ''}（${viaLabel(i)}，${i.state === 'running' ? '运行中' : '已停止'}${i.listen?.length ? '，' + i.listen.join(' ') : ''}）`).join('；') }}。
          直接用它的话不用再装；要另装一个独立的，继续往下填（端口已自动避开）。
        </template>
      </el-alert>
      <el-form label-width="120px" class="form">
        <el-form-item label="装法">
          <el-select v-model="vi" class="wide">
            <el-option v-for="(v, i) in tpl.variants" :key="i" :value="i" :label="v.label" :disabled="!!variantMisfit(v, os, arch)">
              <span>{{ v.label }}</span>
              <span class="muted small opt-why">{{ variantMisfit(v, os, arch) || howTo(v) }}</span>
            </el-option>
          </el-select>
          <div v-if="misfit" class="err small">{{ misfit }}</div>
          <div v-if="variant.note" class="muted small">{{ variant.note }}</div>
          <div v-if="variant.system && !variant.package" class="muted small">
            从服务器的软件源安装：{{ [...(variant.system.dnf ?? []).map((p) => `dnf ${p.split('|').join(' 或 ')}`), ...(variant.system.apt ?? []).map((p) => `apt ${p.split('|').join(' 或 ')}`)].join('、') }}；由 Ferry 单独运行一个实例
          </div>
        </el-form-item>

        <el-form-item v-if="downloads" label="下载走">
          <el-radio-group v-model="form.network" size="small">
            <el-radio-button value="auto">自动</el-radio-button>
            <el-radio-button value="local">这台电脑的网络</el-radio-button>
            <el-radio-button value="server">服务器的网络</el-radio-button>
          </el-radio-group>
          <div class="muted small">
            <template v-if="form.network === 'auto'">先看服务器连不连得上它的软件源，连不上的经这台电脑下载</template>
            <template v-else-if="form.network === 'local'">服务器上的 dnf / yum / apt 和安装步骤通过 SSH 用这台电脑的网络下载；依赖照常由服务器算，装完就断开</template>
            <template v-else>服务器自己下载（要能访问软件源或内网镜像）</template>
          </div>
        </el-form-item>

        <el-form-item v-if="variant.package?.url" label="版本">
          <el-input v-model="form.version" class="mono short" :placeholder="latestLoading ? '正在查最新版本…' : latest || variant.package.fallback" />
          <span class="muted small ml">
            <template v-if="latestLoading">正在查最新版本…</template>
            <template v-else-if="latest">最新 {{ latest }}</template>
            <template v-else>查不到最新版本，不填用 {{ variant.package.fallback }}</template>
          </span>
          <div class="muted small">
            <template v-if="localPkg">本机已有 {{ localPkg.file }}，直接上传</template>
            <template v-else>
              安装时先下载（{{ variant.package.size || '大小未知' }}）<template v-if="variant.package.github">；GitHub 慢的话可以在设置 → 运行环境里设置加速</template>，或
              <el-button link type="primary" size="small" :icon="Upload" @click="importPkg((m) => m.template === tpl?.id && (form.version = m.version ?? ''))">
                导入下载好的安装包
              </el-button>
            </template>
          </div>
        </el-form-item>
        <el-form-item v-else-if="variant.package" label="安装包" :error="needPkg ? '先导入安装包，再选它' : ''">
          <el-select v-model="form.pkg" class="wide" placeholder="选本机库里的安装包" filterable>
            <el-option v-for="m in library" :key="m.file" :value="m.file" :label="m.desc ? `${m.file}（${m.desc}）` : m.file" />
          </el-select>
          <el-button link type="primary" :icon="Upload" class="ml" @click="importPkg((m) => (form.pkg = m.file))">导入…</el-button>
        </el-form-item>

        <el-form-item label="名称" :error="nameError">
          <el-input v-model="form.name" class="mono short" />
        </el-form-item>
        <el-form-item label="安装位置" :error="inError">
          <el-input v-model="form.in" class="mono" placeholder="/opt/ferry-software" />
          <div class="muted small">
            装到 <span class="mono">{{ installDir }}</span>（文件夹名是名称加版本），程序、配置、数据、日志都放在里面。
            <template v-if="disk">所在分区（{{ disk.path }}）剩余 {{ gb(disk.freeMB) }}。</template>
            <el-button v-if="roomier" link type="primary" size="small" @click="form.in = `${roomier.path}/ferry-software`">
              改放到 {{ roomier.path }}（剩余 {{ gb(roomier.freeMB) }}）
            </el-button>
          </div>
        </el-form-item>

        <el-form-item v-for="p in params" :key="p.key" :label="p.type === 'bool' ? '' : p.label || p.key" :error="paramError(p)">
          <el-input-number v-if="p.type === 'port' || p.type === 'number'" v-model="values[p.key] as number" :min="1" :max="65535" :controls="false" class="port" />
          <el-checkbox v-else-if="p.type === 'bool'" v-model="values[p.key] as boolean">{{ p.label || p.key }}</el-checkbox>
          <el-select v-else-if="p.type === 'memory'" v-model="values[p.key] as string" class="short" filterable allow-create>
            <el-option v-for="m in memOptions" :key="m" :value="m" :label="m" />
          </el-select>
          <el-select v-else-if="p.type === 'select'" v-model="values[p.key] as string" class="short">
            <el-option v-for="o in p.options ?? []" :key="o" :value="o" :label="o" />
          </el-select>
          <el-input v-else-if="p.type === 'password'" v-model="values[p.key] as string" show-password class="mono" placeholder="不填 = 随机生成" />
          <el-input v-else v-model="values[p.key] as string" class="mono" />
          <span v-if="p.type === 'memory' && report?.memAvailMB" class="muted small ml">服务器内存 {{ gb(report.memTotalMB ?? 0) }}，现在可用约 {{ gb(report.memAvailMB) }}</span>
          <el-button v-if="p.type === 'port' && paramError(p) && nextFree(p)" link type="primary" size="small" class="ml" @click="values[p.key] = nextFree(p)">换成 {{ nextFree(p) }}</el-button>
          <div v-if="p.help || p.type === 'password'" class="muted small">
            {{ p.help }}<template v-if="p.type === 'password' && p.key === 'password'">{{ p.help ? '。' : '' }}装好后显示，并存进本机的系统凭据库</template>
          </div>
          <div v-if="hints[p.key] && hints[p.key].value === values[p.key]" class="muted small">{{ hints[p.key].text }}</div>
          <div v-if="p.type === 'memory' && memWarn(p)" class="err small">{{ memWarn(p) }}</div>
        </el-form-item>

        <el-form-item label="运行用户">
          <el-input v-model="form.user" class="mono short" />
          <span class="muted small ml">没有会自动创建（不能登录的系统用户）</span>
        </el-form-item>
        <el-form-item v-if="variant.java" label="JDK">
          <el-select v-model="form.java" class="short">
            <el-option v-for="n in [8, 11, 17, 21, 25]" :key="n" :value="n" :label="`Java ${n}`" />
          </el-select>
          <span class="muted small ml">{{ jdkReady(form.java) && !form.javaDir.trim() ? '服务器上已有 Ferry 提供的' : '会先装到服务器上（本机没有时先下载，约 190 MB）' }}</span>
        </el-form-item>
        <el-form-item v-if="variant.java" label="JDK 安装目录" :error="javaDirError">
          <el-input v-model="form.javaDir" class="mono" clearable :placeholder="`留空：${sharedJdk(form.java)}（公共目录）`" />
          <div class="muted small">{{ javaDirHint(form.java) }}</div>
        </el-form-item>
        <el-form-item label="安装时会">
          <ul class="summary small">
            <li v-for="s in summary" :key="s">{{ s }}</li>
          </ul>
        </el-form-item>
      </el-form>
    </template>

    <!-- 自定义安装 -->
    <template v-else-if="view === 'custom'">
      <p class="muted small desc-line">
        把自己的程序或压缩包装到服务器上，作为系统服务 <span class="mono">ferry-{{ custom.name }}</span> 运行。如果是你们自己经常改版本的程序，放在「应用」里部署更合适（有版本和回滚）。
      </p>
      <el-form label-width="120px" class="form">
        <el-form-item label="安装包" :error="shownErrors.pkg">
          <el-select v-model="custom.pkg" class="wide" placeholder="选本机库里的安装包" filterable>
            <el-option v-for="m in library" :key="m.file" :value="m.file" :label="m.desc ? `${m.file}（${m.desc}）` : m.file" />
          </el-select>
          <el-button link type="primary" :icon="Upload" class="ml" @click="importPkg((m) => (custom.pkg = m.file))">导入…</el-button>
          <div v-if="customFile" class="muted small">
            格式
            <el-select v-model="custom.format" size="small" class="fmt">
              <el-option v-for="f in ['binary', 'jar', 'tar.gz', 'tar.xz', 'zip']" :key="f" :value="f" :label="f" />
            </el-select>
            <template v-if="isArchive">
              ，解压时去掉 <el-input-number v-model="custom.strip" :min="0" :max="5" size="small" class="strip" /> 层目录（压缩包里通常有一层「软件名-版本/」）
            </template>
            <template v-else>，放在安装目录下的 <span class="mono">{{ custom.into }}</span></template>
          </div>
        </el-form-item>
        <el-form-item label="显示名">
          <el-input v-model="custom.title" class="short" placeholder="如 我的服务" />
        </el-form-item>
        <el-form-item label="名称" :error="shownErrors.name">
          <el-input v-model="custom.name" class="mono short" />
        </el-form-item>
        <el-form-item label="安装位置" :error="shownErrors.in">
          <el-input v-model="form.in" class="mono" placeholder="/opt/ferry-software" />
          <div class="muted small">
            装到 <span class="mono">{{ installDir }}</span><template v-if="shownVersion">（文件夹名是名称加版本）</template><template v-else>（安装包没有版本，文件夹名就是名称）</template>。
            <template v-if="disk">所在分区（{{ disk.path }}）剩余 {{ gb(disk.freeMB) }}</template>
          </div>
        </el-form-item>
        <el-form-item label="启动命令" :error="shownErrors.exec">
          <el-input v-model="custom.exec" type="textarea" :rows="3" class="mono" />
          <div class="muted small">
            可以用 <span class="mono">${dir}</span>（安装目录）、<span class="mono">${port}</span>（下面的端口）、<span class="mono">${java}</span>（选了 JDK 时）。要在前台运行，不要用 nohup、&amp; 放到后台
          </div>
        </el-form-item>
        <el-form-item label="端口" :error="shownErrors.port">
          <el-input-number v-model="custom.port" :min="1" :max="65535" :controls="false" class="port" />
          <span class="muted small ml">启动后等这个端口能连上，算启动成功</span>
        </el-form-item>
        <el-form-item label="JDK">
          <el-select v-model="custom.java" class="short">
            <el-option :value="0" label="不需要" />
            <el-option v-for="n in [8, 11, 17, 21, 25]" :key="n" :value="n" :label="`Java ${n}`" />
          </el-select>
        </el-form-item>
        <el-form-item v-if="custom.java" label="JDK 安装目录" :error="javaDirError">
          <el-input v-model="form.javaDir" class="mono" clearable :placeholder="`留空：${sharedJdk(custom.java)}（公共目录）`" />
          <div class="muted small">{{ javaDirHint(custom.java) }}</div>
        </el-form-item>
        <el-form-item label="运行用户">
          <el-input v-model="custom.user" class="mono short" :placeholder="custom.name.replace(/-/g, '_')" />
        </el-form-item>
        <el-form-item label="存成模板" :error="shownErrors.id">
          <el-checkbox v-model="custom.save">以后在软件池里一键装</el-checkbox>
          <el-input v-if="custom.save" v-model="custom.id" class="mono short ml" placeholder="模板 ID，如 my-app" />
        </el-form-item>
      </el-form>
    </template>

    <template #footer>
      <el-button @click="emit('close')">取消</el-button>
      <el-button v-if="view === 'template'" type="primary" :disabled="!canInstall" @click="install">安装</el-button>
      <el-button v-if="view === 'custom'" type="primary" @click="installCustom">安装</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.head {
  display: flex;
  align-items: center;
  gap: 12px;
}
.title {
  font-size: 16px;
  font-weight: 500;
}
.bar {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-bottom: 12px;
  flex-wrap: wrap;
}
.spacer {
  flex: 1;
}
.search {
  width: 180px;
}
.tpl-err {
  margin-bottom: 8px;
}
.grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(260px, 1fr));
  gap: 10px;
}
.card {
  border: 1px solid var(--el-border-color-light);
  border-radius: 8px;
  padding: 12px 14px;
  cursor: pointer;
  display: flex;
  flex-direction: column;
  gap: 6px;
  transition: border-color 0.15s, box-shadow 0.15s;
}
.card:hover {
  border-color: var(--el-color-primary);
  box-shadow: 0 2px 8px var(--el-color-primary-light-8);
}
.card.off {
  opacity: 0.65;
}
.card.custom {
  border-style: dashed;
}
.card-head {
  display: flex;
  align-items: center;
  gap: 6px;
}
.desc {
  line-height: 1.5;
  display: -webkit-box;
  -webkit-line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
}
.fit .ok {
  color: var(--el-color-success);
}
.foot {
  margin: 12px 0 0;
}
.desc-line {
  margin: 0 0 12px;
  line-height: 1.6;
}
.form :deep(.el-form-item__content) {
  display: block;
}
.wide {
  width: 360px;
}
.short {
  width: 200px;
}
.port {
  width: 120px;
}
.fmt {
  width: 100px;
}
.strip {
  width: 90px;
}
.ml {
  margin-left: 10px;
}
.small {
  font-size: 12px;
  line-height: 1.6;
}
.err {
  color: var(--el-color-danger);
}
.opt-why {
  float: right;
  margin-left: 16px;
}
.summary {
  margin: 0;
  padding-left: 18px;
  color: var(--el-text-color-regular);
}
</style>

<style>
/* 窗口不够高时只在对话框里面滚动，整个窗口不出滚动条 */
.el-dialog.fit-dialog {
  display: flex;
  flex-direction: column;
  max-height: 92vh;
  margin-bottom: 0;
}
.el-dialog.fit-dialog .el-dialog__body {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  overflow-x: hidden;
}
.el-dialog.fit-dialog .el-form-item {
  margin-bottom: 10px;
}
.ai-tpl {
  margin-bottom: 12px;
}
.ai-steps {
  margin: 6px 0 0;
  font-size: 12px;
  white-space: pre-wrap;
  word-break: break-all;
  max-height: 200px;
  overflow: auto;
}
</style>
