<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { FolderOpened } from '@element-plus/icons-vue'
import { call, type LocalReport, type LocalTarget, type LocalVar } from '../ferry'
import { pickDir, pickFile } from '../platform'

// 本机设置：只属于这台电脑的东西，存在 Ferry 的数据目录里，不写进 ferry.yaml。
// ferry.yaml 写通用的配置；这里按服务器分开：顶部选中哪台服务器，就只设置那台的登录方式和变量（JDK、脚本的位置……）。
// 源码目录这类这台电脑上的路径所有服务器共用，单独一栏。
const props = defineProps<{ config: string; focus?: string }>()
const emit = defineEmits<{ close: []; saved: []; keygen: [target: string] }>()

const visible = ref(true)
const loading = ref(true)
const saving = ref(false)
const error = ref('')
const rep = ref<LocalReport>()

type Mode = 'yaml' | 'key' | 'default' | 'password'
type TargetForm = { mode: Mode; key: string; vars: Record<string, string> }
// 全部服务器的都放在表单里（只显示当前这台），保存时别的服务器的设置原样带上
const form = reactive<{ targets: Record<string, TargetForm>; vars: Record<string, string> }>({ targets: {}, vars: {} })

const modeOf = (l: LocalTarget | null): Mode =>
  !l || (!l.auth && !l.identityFile) ? 'yaml' : l.auth === 'password' ? 'password' : l.identityFile ? 'key' : 'default'

async function load() {
  loading.value = true
  error.value = ''
  try {
    const r = await call<LocalReport>(['local', '-c', props.config])
    // ferry.yaml 里没有项目变量、没有服务器时，以前的 ferry 给的是 null
    r.targets ??= []
    r.vars ??= []
    r.problems ??= []
    rep.value = r
    for (const t of r.targets) {
      const vars: Record<string, string> = {}
      for (const v of t.vars ?? []) vars[v.name] = v.local ?? ''
      form.targets[t.name] = { mode: modeOf(t.local), key: t.local?.identityFile ?? '', vars }
    }
    for (const v of r.vars) form.vars[v.name] = v.local ?? ''
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}
onMounted(load)

// 当前这台服务器：顶部选中的
const current = computed(() => rep.value?.targets.find((t) => t.name === props.focus) ?? rep.value?.targets[0])
const tf = computed(() => (current.value ? form.targets[current.value.name] : undefined))

// 用在这台电脑上的变量（源码目录等）：所有服务器共用
const localUse = computed(() => new Set((rep.value?.vars ?? []).filter((v) => v.localUse).map((v) => v.name)))
// 这台服务器的变量
const serverVars = computed<LocalVar[]>(() => (current.value?.vars ?? []).filter((v) => !localUse.value.has(v.name)))
// 所有服务器共用的：这台电脑上的路径，以及以前在这里改过的服务器变量（对所有服务器都生效，提示移到当前服务器）
const commonVars = computed<LocalVar[]>(() => (rep.value?.vars ?? []).filter((v) => v.localUse || form.vars[v.name]?.trim()))
const misplaced = (v: LocalVar) => !v.localUse && !!form.vars[v.name]?.trim() && (rep.value?.targets.length ?? 0) > 1

// 把所有服务器共用的值改成只对当前这台服务器
function moveToServer(name: string) {
  if (!tf.value) return
  if (!tf.value.vars[name]?.trim()) tf.value.vars[name] = form.vars[name].trim()
  form.vars[name] = ''
}

function sharedLogin(t: LocalReport['targets'][number]): string {
  if (t.sharedAuth === 'password') return '密码登录'
  return t.sharedIdentityFile ? `私钥 ${t.sharedIdentityFile}` : '默认私钥 / ssh-agent'
}

// ferry.yaml 指定的私钥在这台电脑上没有（按 ferry.yaml 时才提示）
const sharedKeyMissing = (t: LocalReport['targets'][number]) => !t.local && t.keyMissing

async function chooseKey(name: string) {
  const p = await pickFile('选择这台电脑上的私钥', '私钥', [])
  if (p) {
    form.targets[name].key = p
    form.targets[name].mode = 'key'
  }
}

async function chooseDir(name: string) {
  const p = await pickDir(`${name} 在这台电脑上的位置`)
  if (p) form.vars[name] = p
}

const keyEmpty = (name: string) => form.targets[name]?.mode === 'key' && !form.targets[name].key.trim()
const invalid = computed(() => Object.keys(form.targets).some(keyEmpty))

const problemVars = computed(() => new Set((rep.value?.problems ?? []).flatMap((p) => p.vars ?? [])))
// 这台电脑上缺的：别的服务器的私钥不在这里提
const problems = computed(() => (rep.value?.problems ?? []).filter((p) => p.kind !== 'key-missing' || p.target === current.value?.name))

async function save() {
  if (invalid.value) return
  const value: { targets: Record<string, LocalTarget>; vars: Record<string, string> } = { targets: {}, vars: {} }
  for (const [name, t] of Object.entries(form.targets)) {
    const lt: LocalTarget = {}
    if (t.mode === 'key') Object.assign(lt, { auth: 'key', identityFile: t.key.trim() })
    else if (t.mode === 'default') lt.auth = 'key'
    else if (t.mode === 'password') lt.auth = 'password'
    const vars: Record<string, string> = {}
    for (const [k, v] of Object.entries(t.vars)) if (v.trim()) vars[k] = v.trim()
    if (Object.keys(vars).length) lt.vars = vars
    if (Object.keys(lt).length) value.targets[name] = lt
  }
  for (const [k, v] of Object.entries(form.vars)) if (v.trim()) value.vars[k] = v.trim()
  saving.value = true
  error.value = ''
  try {
    await call(['local', 'save', '-c', props.config], { FERRY_LOCAL_VALUE: JSON.stringify(value) })
    ElMessage.success('已保存到这台电脑的本机设置')
    emit('saved')
    visible.value = false
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    saving.value = false
  }
}

function keygen(name: string) {
  emit('keygen', name)
  visible.value = false
}
</script>

<template>
  <el-dialog v-model="visible" :title="current ? `本机设置 · ${current.name}` : '本机设置'" width="760px" top="6vh" @closed="emit('close')">
    <el-skeleton v-if="loading" :rows="5" animated />
    <template v-else-if="rep && current && tf">
      <p class="muted intro">
        ferry.yaml 写通用的配置；这里是 <b class="mono">{{ current.name }}</b> 在这台电脑上的设置，<b>只对这台服务器生效</b>，
        存在 Ferry 的数据目录里，不写进 ferry.yaml。设置别的服务器：在顶部切换服务器后再打开。
      </p>
      <el-alert v-if="problems.length" type="warning" :closable="false" show-icon class="problems" title="这台电脑上缺这些">
        <ul>
          <li v-for="p in problems" :key="p.kind + p.path">{{ p.msg }}</li>
        </ul>
      </el-alert>

      <h4>登录</h4>
      <div class="row">
        <div class="row-head">
          <span class="muted small">ferry.yaml：{{ sharedLogin(current) }}</span>
          <el-tag v-if="sharedKeyMissing(current)" type="danger" size="small" effect="plain">这台电脑上没有这个私钥</el-tag>
        </div>
        <el-radio-group v-model="tf.mode" size="small">
          <el-radio-button value="yaml">按 ferry.yaml</el-radio-button>
          <el-radio-button value="key">这台电脑上的私钥</el-radio-button>
          <el-radio-button value="default">默认私钥 / ssh-agent</el-radio-button>
          <el-radio-button value="password">密码登录</el-radio-button>
        </el-radio-group>
        <div v-if="tf.mode === 'key'" class="key-line">
          <el-input v-model="tf.key" class="mono" size="small" placeholder="私钥文件的位置" />
          <el-button size="small" :icon="FolderOpened" @click="chooseKey(current.name)">选择…</el-button>
        </div>
        <div v-if="keyEmpty(current.name)" class="err small">选一个私钥文件</div>
        <div v-if="tf.mode === 'password'" class="muted small">连接时询问密码，可以记到系统凭据库。</div>
        <div v-if="sharedKeyMissing(current) || tf.mode === 'default'" class="muted small hint">
          这台电脑还没有能登录的私钥？
          <el-button link type="primary" size="small" @click="keygen(current.name)">配置这台电脑的免密登录</el-button>
          （用密码登录一次，生成这台电脑自己的私钥并装到服务器上，自动记到本机设置）
        </div>
      </div>

      <template v-if="serverVars.length">
        <h4>变量</h4>
        <p class="muted small">
          ferry.yaml 里的值在这台服务器上不对时（比如 JDK、脚本、密钥文件放的位置不一样）在这里改，只对 {{ current.name }} 生效。留空 = 按 ferry.yaml。
        </p>
        <div v-for="v in serverVars" :key="v.name" class="var two">
          <span class="mono name" :title="v.name">${{ '{' + v.name + '}' }}</span>
          <el-input v-model="tf.vars[v.name]" class="mono" size="small" :placeholder="v.shared || '（空）'" clearable />
        </div>
      </template>

      <template v-if="commonVars.length">
        <h4>所有服务器共用</h4>
        <p class="muted small">这台电脑上的路径，比如源码目录，部署到哪台服务器都用它。留空 = 按 ferry.yaml。</p>
        <div v-for="v in commonVars" :key="v.name" class="var-wrap">
          <div class="var" :class="{ bad: problemVars.has(v.name) || misplaced(v) }">
            <span class="mono name" :title="v.name">${{ '{' + v.name + '}' }}</span>
            <el-input v-model="form.vars[v.name]" class="mono" size="small" :placeholder="v.shared || '（空）'" clearable />
            <el-button v-if="v.localUse" size="small" :icon="FolderOpened" title="选择文件夹" @click="chooseDir(v.name)" />
            <span v-else />
          </div>
          <div v-if="misplaced(v)" class="small misplaced">
            这是服务器上的路径，写在这里对所有服务器都生效。
            <el-button link type="primary" size="small" @click="moveToServer(v.name)">改成只对 {{ current.name }}</el-button>
          </div>
        </div>
      </template>
      <p class="muted small file">保存在 <span class="mono">{{ rep.file }}</span></p>
    </template>
    <el-alert v-if="error" type="error" :title="error" :closable="false" show-icon class="mt" />
    <template #footer>
      <el-button @click="visible = false">取消</el-button>
      <el-button type="primary" :disabled="loading || !rep || invalid" :loading="saving" @click="save">保存</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.intro {
  margin: 0 0 12px;
  line-height: 1.7;
}
h4 {
  margin: 16px 0 8px;
}
.problems ul {
  margin: 4px 0 0;
  padding-left: 18px;
  line-height: 1.7;
}
.row {
  padding: 4px 0 6px;
}
.row-head {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-bottom: 8px;
  flex-wrap: wrap;
}
.name {
  font-weight: 600;
}
.key-line {
  display: flex;
  gap: 8px;
  margin-top: 8px;
}
.hint {
  margin-top: 6px;
}
.var {
  display: grid;
  grid-template-columns: 140px 1fr auto;
  gap: 8px;
  align-items: center;
  margin-bottom: 8px;
}
.var.two {
  grid-template-columns: 140px 1fr;
}
.var .name {
  overflow: hidden;
  text-overflow: ellipsis;
}
.var.bad .name {
  color: var(--el-color-danger);
}
.misplaced {
  margin: -4px 0 10px 148px;
  color: var(--el-color-warning);
}
.small {
  font-size: 12px;
  line-height: 1.6;
}
.err {
  color: var(--el-color-danger);
  margin-top: 4px;
}
.file {
  margin-top: 14px;
  word-break: break-all;
}
.mt {
  margin-top: 10px;
}
</style>
