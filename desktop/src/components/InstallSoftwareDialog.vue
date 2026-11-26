<script setup lang="ts">
import { computed, defineAsyncComponent, onMounted, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { ArrowLeft, Delete, FolderOpened, Search, Plus, Upload } from '@element-plus/icons-vue'
import { call, variantMisfit, type ArchiveCandidate, type ArchivePreview, type Software, type SoftwareReport, type SwExtra, type SwExtraPreview, type SwPackage, type SwParam, type SwTemplate, type SwTemplates, type SwVariant } from '../ferry'
import { extraLine, extrasCount, extrasPayload, extraWhere, rowErrors, type ExtraRow } from '../softwareExtras'
import { buildCustomYAML, cloneCustomDraft, createCustomDraft, deriveTitleFromFile, detectPackageFormat, isArchiveFormat, isSystemSingleFormat, nextFreeTcpPort, validateCustomDraft, type CustomInstallDraft } from '../customInstall'
import { customInstallRequest, drawerOpen, startTask, type Task } from '../state'
import { pickFile } from '../platform'
import LoadingBlock from './LoadingBlock.vue'
import { readCall, ReadCanceled, useReadRequests } from '../readRequests'
const RemoteDirectoryPicker = defineAsyncComponent(() => import('./RemoteDirectoryPicker.vue'))

const props = defineProps<{
  config: string
  target: string
  report?: SoftwareReport
  done: (t: Task) => void
  initialCustom?: { draft: CustomInstallDraft; error?: string; diagLines?: string[] }
}>()
const emit = defineEmits<{ close: [] }>()
const reads = useReadRequests()
let latestSequence = 0

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
const directoryField = ref<'in' | 'javaDir'>()
watch(() => [props.config, props.target], () => { directoryField.value = undefined })
function selectDirectory(path: string) {
  if (!directoryField.value) return
  form.value[directoryField.value] = path
  directoryField.value = undefined
}
// 服务器要下载东西（从软件源装包、步骤里的下载）时走谁的网络
type Network = 'auto' | 'local' | 'server'
const offlineOption = computed(() => !!variant.value?.system && !variant.value.package)
const offlineBundleKind = computed<'rpm' | 'deb' | 'auto'>(() => {
  const system = variant.value?.system
  if (!system?.apt?.length) return 'rpm'
  if (!system.dnf?.length || props.report?.pkgMgr === 'apt-get') return 'deb'
  if (props.report?.pkgMgr === 'dnf' || props.report?.pkgMgr === 'yum') return 'rpm'
  return 'auto'
})
const offlineBundleLabel = computed(() => offlineBundleKind.value === 'auto' ? 'RPM 或 DEB' : offlineBundleKind.value.toUpperCase())
const offlinePackages = computed(() => library.value.filter(m => m.bundle && (offlineBundleKind.value === 'auto' || m.bundle === offlineBundleKind.value)))
const offlineImportError = ref('')
const offlineImportBusy = ref(false)
const downloads = computed(() => !(offlineOption.value && form.value.pkg) && (!!variant.value?.system || !!variant.value?.steps?.some((st) => st.run)))
const values = ref<Record<string, string | number | boolean>>({})
const builtinNacos = computed(() => tpl.value?.builtin && tpl.value.id === 'nacos')
const visibleParams = computed(() => params.value.filter((p) => !builtinNacos.value || p.key !== 'consolePassword' || !!values.value.consoleUser))

// ---------------------------------------------------------------- 更多配置（模板参数没有的设置）

const extrasOpen = ref(false)
const extraRows = ref<Record<string, ExtraRow[]>>({})
/** 这种装法能用的附加配置（和 Go 的 Extra.appliesTo 一致） */
const extras = computed<SwExtra[]>(() => {
  const v = variant.value
  if (!v) return []
  return (tpl.value?.extras ?? []).filter((x) => {
    if (x.format === 'env' || x.format === 'args') return true
    if (v.files?.some((f) => f.path === x.file)) return true
    return x.format !== 'ini' && !!v.props?.some((pr) => pr.file === x.file)
  })
})
const extraErrors = computed(() => Object.fromEntries(extras.value.map((x) => [x.id, rowErrors(x, extraRows.value[x.id] ?? [])])))
const extrasInvalid = computed(() => Object.values(extraErrors.value).some((es) => es.some((e) => e.key || e.value)))
const extrasFilled = computed(() => extrasCount(Object.fromEntries(extras.value.map((x) => [x.id, extraRows.value[x.id] ?? []]))))
function extrasJSON(): string {
  return extrasPayload(extras.value, extraRows.value)
}
function suggestion(x: SwExtra, key: string) {
  const k = key.trim()
  return (x.suggest ?? []).find((s) => s.key === k)
}
/** 还没填的常用项 */
function unusedSuggest(x: SwExtra) {
  const used = new Set((extraRows.value[x.id] ?? []).map((r) => r.key.trim()))
  return (x.suggest ?? []).filter((s) => !used.has(s.key))
}
function addExtraRow(x: SwExtra, s?: { key: string; example?: string }) {
  const rows = (extraRows.value[x.id] ??= [])
  // 有空行时先用空行
  const blank = rows.find((r) => !r.key.trim() && !r.value.trim())
  if (blank && s) Object.assign(blank, { key: s.key, value: s.example ?? '' })
  else rows.push({ key: s?.key ?? '', value: s?.example ?? '' })
}
function removeExtraRow(x: SwExtra, i: number) {
  extraRows.value[x.id]?.splice(i, 1)
}
function searchSuggest(x: SwExtra, q: string, cb: (items: { value: string; help?: string }[]) => void) {
  const k = q.trim().toLowerCase()
  cb((x.suggest ?? []).filter((s) => !k || s.key.toLowerCase().includes(k)).map((s) => ({ value: s.key, help: s.help })))
}
function pickSuggest(x: SwExtra, row: ExtraRow) {
  const s = suggestion(x, row.key)
  if (s?.example && !row.value.trim()) row.value = s.example
}
function valuePlaceholder(x: SwExtra, key: string): string {
  const s = suggestion(x, key)
  if (s?.example) return `如 ${s.example}`
  if (x.format === 'ini') return '值（只写名称的选项留空）'
  return '值'
}
const latest = ref('')
const latestError = ref('')
const latestLoading = ref(false)

const taken = computed(() => new Set((props.report?.items ?? []).map((i) => i.instance).filter((x): x is string => !!x)))

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
  extraRows.value = {}
  extrasOpen.value = false
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
  offlineImportError.value = ''
  latest.value = ''
  latestError.value = ''
  ++latestSequence
  reads.cancel('latest')
  latestLoading.value = false
  if (v.package?.url) {
    if (!v.package.recommended) loadLatest()
  } else if (v.package) form.value.pkg = library.value.find((m) => m.template === tpl.value?.id)?.file ?? ''
}
watch(vi, applyVariant)

async function loadLatest() {
  const t = tpl.value
  if (!t) return
  const sequence = ++latestSequence
  const variantIndex = vi.value
  reads.cancel('latest')
  latestLoading.value = true
  latestError.value = ''
  try {
    const r = await reads.run('latest', s => readCall<{ version: string }>(['software', 'latest', t.id, '--variant', String(variantIndex + 1)], s))
    if (sequence === latestSequence && tpl.value === t && vi.value === variantIndex) latest.value = r.version
  } catch (e) {
    if (sequence === latestSequence && tpl.value === t && vi.value === variantIndex && !(e instanceof ReadCanceled)) latestError.value = (e as Error).message
  } finally {
    if (sequence === latestSequence) latestLoading.value = false
  }
}

const localPkg = computed(() => {
  const t = tpl.value
  const ver = form.value.version || variant.value?.package?.recommended || latest.value || variant.value?.package?.fallback
  return library.value.find((m) => m.template === t?.id && (!ver || m.version === ver))
})

// 要装的版本（安装目录的文件夹名用）：填的、选的安装包的、查到的最新版本；查不到最新时和 ferry 一样退回本机已有的或备用版本
const shownVersion = computed(() => {
  if (view.value === 'custom') return library.value.find((m) => m.file === custom.value.pkg)?.version ?? ''
  const p = variant.value?.package
  if (!p) return ''
  if (form.value.version) return form.value.version
  if (form.value.pkg) return library.value.find((m) => m.file === form.value.pkg)?.version ?? ''
  if (p.recommended) return p.recommended
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
function installLocationError(path: string): string {
  const d = path.trim().replace(/\/+$/, '')
  if (path.trim() === '/') return '请选择根目录下的文件夹，如 /opt/ferry-software'
  return /^\/[A-Za-z0-9._-]+(\/[A-Za-z0-9._-]+)*$/.test(d) ? '' : '写绝对路径，只能用字母、数字、点、下划线、横线'
}
const inError = computed(() => installLocationError(form.value.in))

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
function javaInstallLocationError(path: string): string {
  const d = path.trim().replace(/\/+$/, '')
  if (!/^\/[A-Za-z0-9._-]+(\/[A-Za-z0-9._-]+)+$/.test(d)) return '写绝对路径，至少两级，只能用字母、数字、点、下划线、横线'
  const dir = installDir.value
  if (d === dir || d.startsWith(dir + '/') || dir.startsWith(d + '/')) return '不能和安装目录重叠'
  return ''
}
const javaDirError = computed(() => form.value.javaDir.trim() ? javaInstallLocationError(form.value.javaDir) : '')
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
  if (offlineOption.value && form.value.pkg) {
    out.push(`上传 ${form.value.pkg}，使用 ZIP 内的系统包及依赖离线安装`)
  } else if (sys?.length) {
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
  for (const x of extras.value) {
    const errs = extraErrors.value[x.id] ?? []
    const rows = (extraRows.value[x.id] ?? []).filter((r, i) => r.key.trim() && !errs[i]?.key && !errs[i]?.value)
    if (rows.length) out.push(`写进 ${extraWhere(x)}：${rows.map((r) => extraLine(x.format, { key: r.key.trim(), value: r.value.trim() })).join('、')}`)
  }
  if (v.selinux) out.push('服务器开着 SELinux 时，给安装目录和端口加上 SELinux 标签（程序才能读写、监听）')
  out.push('启动后等端口能连上；有密码的软件再用设置的账号和密码登录一次，登录不上就撤销安装')
  if (builtinNacos.value && values.value.consoleUser) out.push(`创建普通用户 ${values.value.consoleUser} 并验证登录`)
  return out
})
const pkgMgrApt = computed(() => props.report?.pkgMgr === 'apt-get')
function paramError(p: SwParam): string {
  const v = values.value[p.key]
  if (builtinNacos.value && p.key === 'consoleUser' && v) {
    if (String(v).toLowerCase() === 'nacos') return 'nacos 是内置管理员，请使用其他用户名'
    if (!/^[A-Za-z][A-Za-z0-9_]{2,31}$/.test(String(v))) return '3～32 位，字母开头，只能用字母、数字、下划线'
  }
  if (builtinNacos.value && p.key === 'consolePassword' && !v) return '请填写普通用户密码'
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
  () => tpl.value && !misfit.value && !nameError.value && !inError.value && !needPkg.value && !javaDirError.value && !extrasInvalid.value && visibleParams.value.every((p) => !paramError(p)),
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

async function importOfflineBundle() {
  const selectedForm = form.value
  const selectedVariant = variant.value
  const kind = offlineBundleKind.value
  const isCurrent = () => view.value === 'template' && form.value === selectedForm && variant.value === selectedVariant
  offlineImportBusy.value = true
  try {
    const file = await pickFile('选择离线系统包 ZIP', 'ZIP 压缩包', ['zip'])
    if (!file || !isCurrent()) return
    offlineImportError.value = ''
    const m = await call<SwPackage>(['software', 'lib', 'import', file, kind])
    if (info.value) info.value.library = [...library.value.filter(p => p.file !== m.file), m]
    if (isCurrent()) {
      selectedForm.pkg = m.file
      ElMessage.success(`已导入并选中 ${m.file}`)
    }
  } catch (e) {
    if (isCurrent()) offlineImportError.value = (e as Error).message
  } finally {
    offlineImportBusy.value = false
  }
}

/** 交给 ferry 的环境变量：参数、附加配置 */
function installEnv(): Record<string, string> {
  const env: Record<string, string> = { FERRY_SOFTWARE_PARAMS: paramsJSON() }
  const extra = extrasJSON()
  if (extra) env.FERRY_SOFTWARE_EXTRA = extra
  return env
}

function paramsJSON(): string {
  const out: Record<string, string> = {}
  for (const p of visibleParams.value) {
    const v = values.value[p.key]
    if (p.type === 'password' && !v) continue // 不填 = 随机生成
    out[p.key] = String(v)
  }
  return JSON.stringify(out)
}

function templateArgs(action: 'plan' | 'install') {
  const t = tpl.value
  if (!t) return
  const f = form.value
  const args = ['software', action, props.target, t.id, '--name', f.name, '--in', f.in.trim().replace(/\/+$/, ''), '--user', f.user, '--variant', String(vi.value + 1)]
  // 版本和界面上显示的文件夹名一致
  const ver = f.version || (variant.value?.package?.url ? shownVersion.value : '')
  if (ver) args.push('--version', ver)
  if (f.pkg) args.push('--package', f.pkg)
  if (variant.value?.java && f.java && f.java !== variant.value.java) args.push('--java', String(f.java))
  if (variant.value?.java && f.javaDir.trim()) args.push('--java-dir', f.javaDir.trim().replace(/\/+$/, ''))
  if (downloads.value && f.network !== 'auto') args.push('--network', f.network)
  args.push('-c', props.config)
  return args
}

const reviewing = ref(false)
const reviewText = ref('')
const reviewError = ref('')
const reviewExtras = ref<SwExtraPreview[]>([])
const extraFile = ref<SwExtraPreview>()
let reviewSequence = 0
watch([form, values, tpl, vi, view, extraRows, () => props.config, () => props.target], () => {
  reviewSequence++
  reads.cancel('software-review')
  reviewing.value = false
  reviewText.value = ''
  reviewError.value = ''
  reviewExtras.value = []
}, { deep: true })

async function review() {
  const args = templateArgs('plan')
  if (!args) return
  const sequence = ++reviewSequence
  reviewing.value = true
  reviewText.value = ''
  reviewError.value = ''
  reviewExtras.value = []
  try {
    const env = installEnv()
    const plan = await reads.run('software-review', signal => readCall<{
      dir: string
      network: 'none' | 'local' | 'server' | 'offline'
      missingPackages?: string[]
      packageChanges?: string[]
      package?: string
      extras?: SwExtraPreview[]
    }>(args, signal, env),
      { timeout: 90_000, timeoutMessage: '安装预检超过 90 秒，请检查服务器和软件源后重试' },
    )
    if (sequence !== reviewSequence) return
    const route = { none: '无需从服务器下载依赖', local: '经本机代理', server: '服务器直连软件源', offline: '使用离线包' }[plan.network]
    reviewText.value = [plan.dir, plan.package, route, plan.missingPackages?.length && `待安装 ${plan.missingPackages.join('、')}`,
      plan.packageChanges?.length && `会改动已装软件包 ${plan.packageChanges.join('、')}`].filter(Boolean).join(' · ')
    reviewExtras.value = plan.extras ?? []
  } catch (e) {
    if (sequence === reviewSequence && !(e instanceof ReadCanceled)) reviewError.value = (e as Error).message
  } finally {
    if (sequence === reviewSequence) reviewing.value = false
  }
}

function install() {
  const t = tpl.value
  const args = templateArgs('install')
  if (!t || !args) return
  const f = form.value
  startTask(`在 ${props.target} 上安装 ${t.name}（${f.name}）`, args, props.done, installEnv())
  emit('close')
}

// ---------------------------------------------------------------- 自定义安装

const customFormats = ['binary', 'jar', 'tar.gz', 'tar.xz', 'tar.bz2', 'tar.zst', 'tar', 'zip', 'rpm', 'deb']
const custom = ref<CustomInstallDraft>(createCustomDraft('app'))
const customAdvancedOpen = ref<string[]>([])
const failedBanner = ref<{ error?: string; diagLines: string[] } | null>(null)
const customFile = computed(() => library.value.find((m) => m.file === custom.value.pkg))
const archivePreview = ref<ArchivePreview>()
const archiveInspecting = ref(false)
const isArchive = computed(() => isArchiveFormat(custom.value.format))
const isSingleSysPkg = computed(() => isSystemSingleFormat(custom.value.format))
let suppressPkgReset = false
let suppressStripInspect = false

const customReviewing = ref(false)
const customReviewText = ref('')
const customReviewError = ref('')
let customReviewSeq = 0

watch(
  [custom, () => form.value.in, () => form.value.javaDir],
  () => {
    customReviewSeq++
    reads.cancel('custom-review')
    customReviewing.value = false
    customReviewText.value = ''
    customReviewError.value = ''
  },
  { deep: true },
)

async function inspectCustomArchive(file: string, format: string, stripArg: number | 'auto', autoFillExec: boolean) {
  archivePreview.value = undefined
  reads.cancel('archive')
  if (!file || !isArchiveFormat(format)) return
  archiveInspecting.value = true
  try {
    const result = await reads.run('archive', (s) =>
      readCall<ArchivePreview>(['software', 'lib', 'inspect', file, String(stripArg)], s),
    )
    if (custom.value.pkg !== file || custom.value.format !== format) return
    archivePreview.value = result
    if (stripArg === 'auto' && result.recommendedStrip !== custom.value.strip) {
      suppressStripInspect = true
      custom.value.strip = result.recommendedStrip
    }
    if (autoFillExec && result.candidates?.length && !custom.value.exec.trim()) {
      applyCandidate(result.candidates[0])
    }
  } catch {
    // 实际安装与预检仍会独立校验安装包。
  } finally {
    archiveInspecting.value = false
  }
}

function applyCandidate(c: ArchiveCandidate) {
  custom.value.exec = c.exec
  if (c.kind === 'jar' && !custom.value.java) {
    custom.value.java = 17
  }
}

watch(
  () => custom.value.pkg,
  (file, prev) => {
    if (!file) {
      archivePreview.value = undefined
      return
    }
    if (suppressPkgReset) {
      suppressPkgReset = false
      void inspectCustomArchive(file, custom.value.format, custom.value.strip, false)
      return
    }
    const fmt = detectPackageFormat(file)
    custom.value.format = fmt
    custom.value.into = fmt === 'jar' || fmt === 'binary' ? file : ''
    // 切换安装包时清理旧包残留的显示名与启动命令
    if (!prev || prev !== file) {
      custom.value.title = deriveTitleFromFile(file)
      if (fmt === 'jar') {
        custom.value.exec = `\${java} -jar \${dir}/${file} --server.port=\${port}`
        if (!custom.value.java) custom.value.java = 17
      } else if (fmt === 'binary') {
        custom.value.exec = `\${dir}/${file}`
      } else {
        custom.value.exec = ''
      }
    }
    if (isArchiveFormat(fmt)) {
      void inspectCustomArchive(file, fmt, 'auto', true)
    } else {
      archivePreview.value = undefined
    }
  },
)

watch(
  () => [custom.value.format, custom.value.strip] as const,
  ([format, strip], [prevFmt, prevStrip]) => {
    if (suppressStripInspect) {
      suppressStripInspect = false
      return
    }
    if (format === prevFmt && strip === prevStrip) return
    if (!custom.value.pkg || !isArchiveFormat(format)) {
      archivePreview.value = undefined
      return
    }
    void inspectCustomArchive(custom.value.pkg, format, strip ?? 0, false)
  },
)

function openCustom() {
  const freePort = nextFreeTcpPort(8080, used.value)
  custom.value = createCustomDraft(freeName('app'), form.value.in || '/opt/ferry-software', freePort)
  customAdvancedOpen.value = []
  failedBanner.value = null
  tried.value = false
  form.value.javaDir = ''
  view.value = 'custom'
}

function restoreCustomFromFailed(req: NonNullable<typeof props.initialCustom>) {
  suppressPkgReset = true
  suppressStripInspect = true
  const draft = cloneCustomDraft(req.draft)
  // 如果上次失败的实例名已被其他软件占用，自动换个可用名；通常失败已回滚，名称可沿用
  if (taken.value.has(draft.name)) draft.name = freeName(draft.name)
  custom.value = draft
  form.value.in = draft.inDir || '/opt/ferry-software'
  form.value.javaDir = draft.javaDir || ''
  failedBanner.value = { error: req.error, diagLines: req.diagLines ?? [] }
  const hasAdv =
    draft.envRows.length > 0 ||
    !!draft.configPath.trim() ||
    !!draft.logPath.trim() ||
    !!draft.workdir.trim() ||
    draft.serviceType === 'forking' ||
    draft.allowRoot ||
    draft.keepOnFail ||
    (draft.readyTimeout > 0 && draft.readyTimeout !== 180)
  customAdvancedOpen.value = hasAdv ? ['adv'] : []
  tried.value = false
  view.value = 'custom'
  if (draft.pkg && isArchiveFormat(draft.format)) {
    void inspectCustomArchive(draft.pkg, draft.format, draft.strip, false)
  }
}

onMounted(() => {
  if (props.initialCustom) restoreCustomFromFailed(props.initialCustom)
})

function addCustomEnv() {
  custom.value.envRows.push({ key: '', value: '' })
}
function removeCustomEnv(i: number) {
  custom.value.envRows.splice(i, 1)
}

// 点了「安装」或「预检」之后才把没填好的标红
const tried = ref(false)
const shownErrors = computed<Record<string, string>>(() => (tried.value ? customErrors.value : {}))

const customErrors = computed(() =>
  validateCustomDraft(custom.value, {
    takenNames: taken.value,
    serverArch: arch.value,
    archivePreview: archivePreview.value,
    inError: inError.value,
    javaDirError: javaDirError.value,
    portTakenText: (p) => portTaken('tcp', p),
  }),
)

/** 自定义安装生成的模板 */
function customYAML(): string {
  return buildCustomYAML(custom.value)
}

function customArgs(action: 'plan' | 'install', tplArg = '-') {
  const c = custom.value
  const args = ['software', action, props.target, tplArg, '--name', c.name, '--in', form.value.in.trim().replace(/\/+$/, ''), '--package', c.pkg, '-c', props.config]
  if (c.java && form.value.javaDir.trim()) args.push('--java-dir', form.value.javaDir.trim().replace(/\/+$/, ''))
  return args
}

async function reviewCustom() {
  tried.value = true
  if (Object.keys(customErrors.value).length > 0) return
  const seq = ++customReviewSeq
  customReviewing.value = true
  customReviewText.value = ''
  customReviewError.value = ''
  try {
    const yaml = customYAML()
    const plan = await reads.run(
      'custom-review',
      (signal) =>
        readCall<{
          dir: string
          exec?: string
          readyMode?: string
          readyPort?: number
          readyUrl?: string
          freeMB?: number
          network: 'none' | 'local' | 'server' | 'offline'
        }>(customArgs('plan', '-'), signal, { FERRY_SOFTWARE_TEMPLATE: yaml }),
      { timeout: 60_000, timeoutMessage: '预检超时，请检查服务器连接后重试' },
    )
    if (seq !== customReviewSeq) return
    const readyDesc =
      plan.readyMode === 'process'
        ? '仅检测进程稳定存活'
        : plan.readyMode === 'http'
          ? `HTTP 检查 ${plan.readyUrl || ''}`
          : `TCP 端口 ${plan.readyPort || custom.value.port}`
    const freeDesc = plan.freeMB && plan.freeMB > 0 ? `分区剩余 ${gb(plan.freeMB)}` : ''
    customReviewText.value = [`预检通过`, plan.dir, readyDesc, freeDesc, plan.exec ? `启动：${plan.exec}` : ''].filter(Boolean).join(' · ')
  } catch (e) {
    if (seq === customReviewSeq && !(e instanceof ReadCanceled)) {
      customReviewError.value = (e as Error).message
    }
  } finally {
    if (seq === customReviewSeq) customReviewing.value = false
  }
}

async function installCustom() {
  tried.value = true
  if (Object.keys(customErrors.value).length > 0) return
  const c = custom.value
  const yaml = customYAML()
  const snap = cloneCustomDraft({
    ...c,
    inDir: form.value.in.trim().replace(/\/+$/, '') || '/opt/ferry-software',
    javaDir: form.value.javaDir.trim().replace(/\/+$/, ''),
  })
  const args = customArgs('install', '-')
  const target = props.target
  const config = props.config
  const task = startTask(
    `在 ${target} 上安装 ${c.title || c.name}（${c.name}）`,
    args,
    async (t) => {
      // 仅在安装成功后保存为模板，避免失败的半成品污染软件池
      if (t.status === 'ok' && snap.save && snap.id) {
        try {
          await call(['software', 'template', 'save'], { FERRY_SOFTWARE_TEMPLATE: yaml })
          ElMessage.success(`已保存为模板 ${snap.id}，以后在软件池里能找到`)
        } catch (e) {
          ElMessage.warning(`软件已安装成功，但保存模板失败：${(e as Error).message}`)
        }
      }
      props.done(t)
    },
    { FERRY_SOFTWARE_TEMPLATE: yaml },
  )
  task.editRetry = () => {
    const diagLines = task.lines
      .filter((l) => l.kind === 'error' || l.kind === 'warn' || l.code === 'diag' || l.code === 'crash')
      .map((l) => l.msg)
      .filter((m): m is string => !!m)
      .slice(-6)
    customInstallRequest.value = { config, target, draft: cloneCustomDraft(snap), error: task.error, diagLines }
    drawerOpen.value = false
  }
  emit('close')
}
</script>

<template>
  <!-- 弹层挂到 body，避免被项目列表的液态玻璃遮罩裁剪。 -->
  <el-dialog :model-value="true" append-to-body width="920px" top="4vh" class="fit-dialog" @close="emit('close')">
    <template #header>
      <div class="head">
        <el-button v-if="view !== 'store'" :icon="ArrowLeft" link @click="view = 'store'">软件池</el-button>
        <span class="el-dialog__title">
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
        <div class="install-template-card custom" @click="openCustom">
          <div class="card-head">
            <el-icon><Plus /></el-icon><b>自定义安装</b>
          </div>
          <div class="muted small">选一个自己的程序或压缩包，填启动命令和端口，装成系统服务；可以存成模板以后再用</div>
        </div>
        <div v-for="t in shown" :key="t.id" class="install-template-card" :class="{ off: bestVariant(t) < 0 }" @click="open(t)">
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
          <el-input v-model="form.version" class="mono short" :placeholder="latestLoading ? '正在查最新版本…' : variant.package.recommended || latest || variant.package.fallback" />
          <span class="muted small ml">
            <template v-if="variant.package.recommended">推荐 {{ variant.package.recommended }}</template>
            <template v-else-if="latestLoading">正在查最新版本…</template>
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
        <el-form-item v-else-if="offlineOption" label="离线系统包" :error="offlineImportError">
          <el-select v-model="form.pkg" class="wide" placeholder="使用系统软件源" filterable clearable @change="offlineImportError = ''">
            <el-option v-for="m in offlinePackages" :key="m.file" :value="m.file" :label="m.file" />
          </el-select>
          <el-button link type="primary" :icon="Upload" class="ml" :loading="offlineImportBusy" @click="importOfflineBundle">导入 ZIP…</el-button>
          <div class="muted small">ZIP 内应包含适合这台服务器的 {{ offlineBundleLabel }} 软件包及其依赖。</div>
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
          <el-input v-model="form.in" class="mono" placeholder="/opt/ferry-software" aria-label="安装位置">
            <template #append><el-button :icon="FolderOpened" @click="directoryField = 'in'">选择服务器目录</el-button></template>
          </el-input>
          <div class="muted small">
            装到 <span class="mono">{{ installDir }}</span>（文件夹名是名称加版本），程序、配置、数据、日志都放在里面。
            <template v-if="disk">所在分区（{{ disk.path }}）剩余 {{ gb(disk.freeMB) }}。</template>
            <el-button v-if="roomier" link type="primary" size="small" @click="form.in = `${roomier.path}/ferry-software`">
              改放到 {{ roomier.path }}（剩余 {{ gb(roomier.freeMB) }}）
            </el-button>
          </div>
        </el-form-item>

        <el-form-item v-for="p in visibleParams" :key="p.key" :label="p.type === 'bool' ? '' : p.label || p.key" :error="paramError(p)">
          <el-input-number v-if="p.type === 'port' || p.type === 'number'" v-model="values[p.key] as number" :min="1" :max="65535" :controls="false" class="port" />
          <el-checkbox v-else-if="p.type === 'bool'" v-model="values[p.key] as boolean">{{ p.label || p.key }}</el-checkbox>
          <el-select v-else-if="p.type === 'memory'" v-model="values[p.key] as string" class="short" filterable allow-create>
            <el-option v-for="m in memOptions" :key="m" :value="m" :label="m" />
          </el-select>
          <el-select v-else-if="p.type === 'select'" v-model="values[p.key] as string" class="short">
            <el-option v-for="o in p.options ?? []" :key="o" :value="o" :label="o" />
          </el-select>
          <el-input v-else-if="p.type === 'password'" v-model="values[p.key] as string" show-password class="mono" :placeholder="builtinNacos && p.key === 'consolePassword' ? '填写普通用户密码' : '不填 = 随机生成'" />
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
          <el-input v-model="form.javaDir" class="mono" clearable :placeholder="`留空：${sharedJdk(form.java)}（公共目录）`" aria-label="JDK 安装目录">
            <template #append><el-button :icon="FolderOpened" aria-label="为 JDK 安装目录选择服务器目录" @click="directoryField = 'javaDir'">选择服务器目录</el-button></template>
          </el-input>
          <div class="muted small">{{ javaDirHint(form.java) }}</div>
        </el-form-item>
        <el-form-item v-if="extras.length" label="更多配置">
          <el-button v-if="!extrasOpen" link type="primary" @click="extrasOpen = true">
            补充模板参数里没有的设置（可选）<template v-if="extrasFilled">：已填 {{ extrasFilled }} 项</template>
          </el-button>
          <div v-else class="extras">
            <div v-for="x in extras" :key="x.id" class="extra">
              <div class="small">
                <b>{{ x.label }}</b>
              </div>
              <div v-if="x.help" class="muted small">{{ x.help }}</div>
              <div v-for="(r, i) in extraRows[x.id] ?? []" :key="i" class="extra-row">
                <div class="extra-line">
                  <div class="extra-key">
                  <el-autocomplete v-model="r.key" :fetch-suggestions="(q: string, cb: any) => searchSuggest(x, q, cb)" placeholder="名称"
                    :aria-label="`${x.label}：第 ${i + 1} 项的名称`" @select="pickSuggest(x, r)" @blur="pickSuggest(x, r)">
                    <template #default="{ item }">
                      <span class="mono">{{ item.value }}</span><span v-if="item.help" class="muted small ml">{{ item.help }}</span>
                    </template>
                  </el-autocomplete>
                  </div>
                  <span class="muted">{{ x.format === 'keyvalue' ? '' : '=' }}</span>
                  <div class="extra-val">
                    <el-input v-model="r.value" class="mono" :placeholder="valuePlaceholder(x, r.key)" :aria-label="`${x.label}：第 ${i + 1} 项的值`" />
                  </div>
                  <el-button link :icon="Delete" :aria-label="`删除第 ${i + 1} 项`" @click="removeExtraRow(x, i)" />
                </div>
                <div v-if="extraErrors[x.id]?.[i]?.key || extraErrors[x.id]?.[i]?.value" class="err small">
                  {{ extraErrors[x.id][i].key || extraErrors[x.id][i].value }}
                </div>
                <div v-else-if="suggestion(x, r.key)?.help" class="muted small">{{ suggestion(x, r.key)?.help }}</div>
              </div>
              <div class="extra-add">
                <el-button link type="primary" size="small" :icon="Plus" @click="addExtraRow(x)">添加一项</el-button>
                <template v-if="unusedSuggest(x).length">
                  <span class="muted small ml">常用：</span>
                  <el-tooltip v-for="sg in unusedSuggest(x)" :key="sg.key" :content="sg.help || sg.key" :show-after="HOVER_DELAY" placement="top">
                    <el-tag class="sug mono" size="small" effect="plain" disable-transitions @click="addExtraRow(x, sg)">{{ sg.key }}</el-tag>
                  </el-tooltip>
                </template>
              </div>
            </div>
            <div class="muted small">同名的项替换模板里的值，没有的加上。端口、密码、目录等用上面的参数设置，这里不能改。点「预检」可以先看生成的配置。</div>
          </div>
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
      <el-alert v-if="failedBanner" type="error" :closable="true" show-icon class="fail-banner" @close="failedBanner = null">
        <template #title>上次安装失败：{{ failedBanner.error || '请根据下方诊断调整参数后重装（已保留原安装包缓存，无需重传）' }}</template>
        <pre v-if="failedBanner.diagLines.length" class="mono fail-diag">{{ failedBanner.diagLines.join('\n') }}</pre>
      </el-alert>
      <p class="muted small desc-line">
        把自己的程序、压缩包或单系统包（RPM/DEB）装到服务器上，作为系统服务 <span class="mono">ferry-{{ custom.name }}</span> 运行。如果是你们自己经常改版本的程序，放在「应用」里部署更合适（有版本和回滚）。
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
              <el-option v-for="f in customFormats" :key="f" :value="f" :label="f" />
            </el-select>
            <template v-if="isArchive">
              ，解压时去掉 <el-input-number v-model="custom.strip" :min="0" :max="5" size="small" class="strip" /> 层目录
              <template v-if="archivePreview">
                （自动推荐 {{ archivePreview.recommendedStrip }} 层<template v-if="archivePreview.singleTopDir">，顶层目录 <span class="mono">{{ archivePreview.singleTopDir }}/</span></template><template v-else>，扁平包</template>）
              </template>
            </template>
            <template v-else-if="isSingleSysPkg">，通过服务器包管理器安装并自动解析依赖（断网可经本机代理）</template>
            <template v-else>，放在安装目录下的 <span class="mono">{{ custom.into }}</span></template>
          </div>
          <div v-if="archiveInspecting" class="muted small">正在探测压缩包结构与程序入口…</div>
          <div v-else-if="archivePreview" class="muted small">
            <template v-if="archivePreview.bundle">内容为 {{ archivePreview.bundle.toUpperCase() }} 软件包集合</template>
            <template v-else>
              解压后 {{ archivePreview.entries }} 个文件
              <template v-if="archivePreview.arches?.length"> · 程序架构 <span class="mono">{{ archivePreview.arches.join('/') }}</span></template>
              <span v-if="archivePreview.files.length"> · {{ archivePreview.files.slice(0, 6).join('、') }}</span>
            </template>
          </div>
          <div v-if="archivePreview?.candidates?.length" class="cand-box">
            <span class="muted small">探测到的入口（点击一键填入启动命令）：</span>
            <div class="cand-list">
              <el-tag
                v-for="c in archivePreview.candidates.slice(0, 6)"
                :key="c.path"
                size="small"
                :type="c.arch && arch && c.arch !== arch ? 'danger' : custom.exec.includes(c.path) ? 'success' : 'info'"
                effect="plain"
                class="cand-tag mono"
                @click="applyCandidate(c)"
              >
                {{ c.path }}
                <template v-if="c.arch"> [{{ c.arch }}]</template>
                <template v-else-if="c.kind === 'jar'"> [jar]</template>
                <template v-if="c.daemon"> · 疑似后台脚本</template>
                <template v-if="c.crlf"> · CRLF(自动转LF)</template>
              </el-tag>
            </div>
          </div>
          <div v-if="archivePreview?.configs?.length" class="muted small">
            包内配置文件：<span class="mono">{{ archivePreview.configs.slice(0, 5).join('、') }}</span>
          </div>
        </el-form-item>
        <el-form-item label="显示名">
          <el-input v-model="custom.title" class="short" placeholder="如 我的服务" />
        </el-form-item>
        <el-form-item label="名称" :error="shownErrors.name">
          <el-input v-model="custom.name" class="mono short" />
        </el-form-item>
        <el-form-item label="安装位置" :error="shownErrors.in">
          <el-input v-model="form.in" class="mono" placeholder="/opt/ferry-software" aria-label="安装位置">
            <template #append><el-button :icon="FolderOpened" @click="directoryField = 'in'">选择服务器目录</el-button></template>
          </el-input>
          <div class="muted small">
            装到 <span class="mono">{{ installDir }}</span><template v-if="shownVersion">（文件夹名是名称加版本）</template><template v-else>（安装包没有版本，文件夹名就是名称）</template>。
            <template v-if="disk">所在分区（{{ disk.path }}）剩余 {{ gb(disk.freeMB) }}</template>
          </div>
        </el-form-item>
        <el-form-item label="启动命令" :error="shownErrors.exec">
          <el-input v-model="custom.exec" type="textarea" :rows="3" class="mono" :placeholder="isSingleSysPkg ? '系统包安装后的可执行路径，如 /usr/bin/mytool --port ${port}' : '${dir}/bin/app --port ${port}'" />
          <div class="muted small">
            可以用 <span class="mono">${dir}</span>（安装目录）、<span class="mono">${port}</span>（端口）、<span class="mono">${java}</span>（选了 JDK 时）。缺执行权限或 CRLF 换行会自动修复；后台守护脚本可在下方「高级选项」切换为 <span class="mono">forking</span>。
          </div>
        </el-form-item>
        <el-form-item label="就绪检查">
          <el-radio-group v-model="custom.readyMode" size="small">
            <el-radio-button value="tcp">TCP 端口</el-radio-button>
            <el-radio-button value="http">HTTP 接口</el-radio-button>
            <el-radio-button value="process">仅检查进程存活（无端口/Worker/UDP）</el-radio-button>
          </el-radio-group>
        </el-form-item>
        <el-form-item v-if="custom.readyMode !== 'process'" label="端口" :error="shownErrors.port">
          <el-input-number v-model="custom.port" :min="1" :max="65535" :controls="false" class="port" />
          <span class="muted small ml">启动后等这个端口就绪（若进程实际监听了别的端口会自动快速提示）</span>
        </el-form-item>
        <el-form-item v-if="custom.readyMode === 'http'" label="HTTP 检查" :error="shownErrors.readyUrl">
          <el-input v-model="custom.readyUrl" class="mono wide" placeholder="http://127.0.0.1:${port}/health" />
        </el-form-item>
        <el-form-item label="JDK">
          <el-select v-model="custom.java" class="short">
            <el-option :value="0" label="不需要" />
            <el-option v-for="n in [8, 11, 17, 21, 25]" :key="n" :value="n" :label="`Java ${n}`" />
          </el-select>
        </el-form-item>
        <el-form-item v-if="custom.java" label="JDK 安装目录" :error="javaDirError">
          <el-input v-model="form.javaDir" class="mono" clearable :placeholder="`留空：${sharedJdk(custom.java)}（公共目录）`" aria-label="JDK 安装目录">
            <template #append><el-button :icon="FolderOpened" aria-label="为 JDK 安装目录选择服务器目录" @click="directoryField = 'javaDir'">选择服务器目录</el-button></template>
          </el-input>
          <div class="muted small">{{ javaDirHint(custom.java) }}</div>
        </el-form-item>
        <el-form-item label="运行用户" :error="shownErrors.user">
          <el-input v-model="custom.user" class="mono short" :disabled="custom.allowRoot" :placeholder="custom.allowRoot ? 'root' : custom.name.replace(/-/g, '_')" />
        </el-form-item>
        <el-form-item label="存成模板" :error="shownErrors.id">
          <el-checkbox v-model="custom.save">安装成功后存为模板（以后在软件池里一键装）</el-checkbox>
          <el-input v-if="custom.save" v-model="custom.id" class="mono short ml" placeholder="模板 ID，如 my-app" />
        </el-form-item>

        <el-collapse v-model="customAdvancedOpen" class="adv-collapse">
          <el-collapse-item title="高级选项（环境变量、配置文件、工作目录、日志、后台模式、排障保留现场）" name="adv">
            <el-form-item label="环境变量" :error="shownErrors.env">
              <div class="env-list">
                <div v-for="(row, i) in custom.envRows" :key="i" class="env-row">
                  <el-input v-model="row.key" class="mono env-k" placeholder="变量名（如 APP_ENV）" />
                  <span class="muted">=</span>
                  <el-input v-model="row.value" class="mono env-v" placeholder="值" />
                  <el-button link :icon="Delete" @click="removeCustomEnv(i)" />
                </div>
                <el-button link type="primary" size="small" :icon="Plus" @click="addCustomEnv">添加环境变量</el-button>
              </div>
            </el-form-item>
            <el-form-item label="写入配置文件" :error="shownErrors.configPath">
              <el-input v-model="custom.configPath" class="mono wide" placeholder="相对路径，如 conf/config.yaml（留空不写）" />
              <el-input
                v-if="custom.configPath.trim()"
                v-model="custom.configContent"
                type="textarea"
                :rows="4"
                class="mono mt-6"
                placeholder="配置文件内容（支持 ${dir}、${port} 变量）"
              />
            </el-form-item>
            <el-form-item label="工作目录">
              <el-input v-model="custom.workdir" class="mono wide" placeholder="留空默认 ${dir}，可填相对路径如 bin" />
            </el-form-item>
            <el-form-item v-if="isArchive" label="解压子目录">
              <el-input v-model="custom.into" class="mono short" placeholder="如 app（填写后支持在线升级）" />
              <span class="muted small ml">把程序单独解到子目录后，未来可直接上传新包升级并自动回滚</span>
            </el-form-item>
            <el-form-item label="日志文件路径">
              <el-input v-model="custom.logPath" class="mono wide" placeholder="安装目录内相对路径，如 logs/app.log（便于失败诊断与查看日志）" />
            </el-form-item>
            <el-form-item label="就绪超时">
              <el-input-number v-model="custom.readyTimeout" :min="15" :max="600" size="small" />
              <span class="muted small ml">秒（默认 180 秒）</span>
            </el-form-item>
            <el-form-item label="启动方式">
              <el-radio-group v-model="custom.serviceType" size="small">
                <el-radio value="simple">前台运行（simple，推荐）</el-radio>
                <el-radio value="forking">后台守护脚本（forking）</el-radio>
              </el-radio-group>
            </el-form-item>
            <el-form-item label="排障与权限">
              <div class="adv-checks">
                <el-checkbox v-model="custom.keepOnFail">失败时保留解压后的安装目录到 <span class="mono">.failed/{{ custom.name }}</span> 以便排查</el-checkbox>
                <el-checkbox v-model="custom.allowRoot">允许以 <span class="mono">root</span> 运行（仅用于确实需要特权的系统采集器/网络工具）</el-checkbox>
              </div>
            </el-form-item>
          </el-collapse-item>
        </el-collapse>
      </el-form>
    </template>

    <template #footer>
      <el-alert v-if="view === 'template' && reviewError" :title="reviewError" type="error" :closable="false" show-icon />
      <div v-if="view === 'template' && reviewText" class="muted small">{{ reviewText }}</div>
      <div v-for="x in view === 'template' ? reviewExtras : []" :key="x.id" class="review-extra small">
        <span class="muted">{{ x.label }}会写进：</span><span class="mono">{{ x.items.map((it) => extraLine(x.format, it)).join('；') }}</span>
        <el-button v-if="x.content || x.exec" link type="primary" size="small" class="ml" @click="extraFile = x">
          {{ x.content ? `看生成的 ${x.file?.split('/').pop()}` : '看启动命令' }}
        </el-button>
      </div>
      <el-alert v-if="view === 'custom' && customReviewError" :title="customReviewError" type="error" :closable="false" show-icon />
      <div v-if="view === 'custom' && customReviewText" class="muted small review-ok">{{ customReviewText }}</div>
      <el-button @click="emit('close')">取消</el-button>
      <el-button v-if="view === 'template'" :disabled="!canInstall" :loading="reviewing" @click="review">预检</el-button>
      <el-button v-if="view === 'template'" type="primary" :disabled="!canInstall || reviewing" @click="install">安装</el-button>
      <el-button v-if="view === 'custom'" :loading="customReviewing" @click="reviewCustom">预检</el-button>
      <el-button v-if="view === 'custom'" type="primary" :disabled="customReviewing" @click="installCustom">安装</el-button>
    </template>
  </el-dialog>
  <el-dialog v-if="extraFile" :model-value="true" append-to-body width="760px" :title="extraFile.content ? extraFile.file : '启动命令'" @close="extraFile = undefined">
    <p class="muted small desc-line">按现在填的参数和更多配置生成；密码已遮住，JDK、安装包的位置是占位。</p>
    <pre class="mono extra-file">{{ extraFile.content || extraFile.exec }}</pre>
  </el-dialog>
  <RemoteDirectoryPicker
    v-if="directoryField"
    :config="config"
    :target="target"
    :path="form[directoryField]"
    :validate="directoryField === 'javaDir' ? javaInstallLocationError : installLocationError"
    @select="selectDirectory"
    @close="directoryField = undefined"
  />
</template>

<style scoped>
.head {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 12px;
  flex-wrap: wrap;
}
.head > .el-dialog__title { flex-basis: 100%; order: -1; }
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
.install-template-card.off {
  opacity: 0.65;
}
.install-template-card.custom {
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
.extras {
  display: flex;
  flex-direction: column;
  gap: 10px;
  max-width: 720px;
}
.extra-row {
  margin-top: 6px;
}
.extra-line {
  display: flex;
  align-items: center;
  gap: 6px;
}
.extra-key {
  flex: 0 0 280px;
}
.extra-key :deep(.el-autocomplete) {
  width: 100%;
}
.extra-key :deep(input) {
  font-family: 'Cascadia Mono', Consolas, 'Courier New', monospace;
  font-size: 12.5px;
}
.extra-val {
  flex: 1;
  min-width: 0;
}
.extra-add {
  margin-top: 4px;
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 4px;
}
.sug {
  cursor: pointer;
}
.review-extra {
  text-align: left;
  margin: 4px 0;
  word-break: break-all;
}
.extra-file {
  margin: 0;
  max-height: 60vh;
  overflow: auto;
  padding: 10px 12px;
  background: var(--el-fill-color-light);
  border-radius: 6px;
  white-space: pre-wrap;
  word-break: break-all;
  font-size: 12px;
}
.summary {
  margin: 0;
  padding-left: 18px;
  color: var(--el-text-color-regular);
}
.fail-banner {
  margin-bottom: 10px;
}
.fail-diag {
  margin: 6px 0 0;
  font-size: 11.5px;
  white-space: pre-wrap;
  word-break: break-all;
  max-height: 140px;
  overflow: auto;
}
.cand-box {
  margin-top: 6px;
}
.cand-list {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
  margin-top: 4px;
}
.cand-tag {
  cursor: pointer;
}
.adv-collapse {
  margin-top: 12px;
}
.env-list {
  display: flex;
  flex-direction: column;
  gap: 6px;
  width: 100%;
  max-width: 660px;
}
.env-row {
  display: flex;
  align-items: center;
  gap: 6px;
}
.env-k {
  flex: 0 0 220px;
}
.env-v {
  flex: 1;
}
.mt-6 {
  margin-top: 6px;
}
.adv-checks {
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.review-ok {
  text-align: left;
  margin-bottom: 6px;
  word-break: break-all;
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
