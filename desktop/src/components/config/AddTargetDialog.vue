<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { appNames } from '../../editor/appSetup'
import { addEntry, hasAlias, keysAt, parse, scalarAt, serialize } from '../../editor/configDoc'
import { readCall, useReadRequests } from '../../readRequests'

const props = defineProps<{ text: string; config: string }>()
const emit = defineEmits<{ close: []; add: [text: string, name: string] }>()
const visible = ref(true)
const form = reactive({
  name: '', title: '', host: '', port: '22', user: 'root', auth: 'key', identityFile: '',
  sudo: true, root: '', runAs: '', apps: [] as string[], secretSource: 'keyring', secretsFile: '',
})
const vars = reactive(new Map<string, string>())
const doc = computed(() => parse(props.text))
const targets = computed(() => keysAt(doc.value, ['targets']))
const deploy = computed(() => scalarAt(doc.value, ['kind']) !== 'connections')
const apps = computed(() => Array.isArray(doc.value.toJS()?.apps) ? appNames(doc.value) : [])
const rootDefault = computed(() => `/opt/${scalarAt(doc.value, ['project']) || 'myapp'}`)
const name = computed(() => form.name.trim())
const nameError = computed(() => {
  if (!name.value) return ''
  if (!/^[a-z0-9][a-z0-9._-]*$/i.test(name.value)) return '用字母或数字开头，可包含 . _ -'
  return targets.value.some((n) => n.toLowerCase() === name.value.toLowerCase()) ? '服务器标识已存在' : ''
})
const portOK = computed(() => /^\d{1,5}$/.test(form.port) && Number(form.port) >= 1 && Number(form.port) <= 65535)
const runAsInvalid = computed(() => deploy.value && !!form.runAs.trim()
  && (form.runAs.trim() === 'root' || !/^[a-z_][a-z0-9_-]*\$?$/.test(form.runAs.trim())))
const nonRoot = computed(() => deploy.value && !!form.user.trim() && form.user.trim() !== 'root')
const documentError = computed(() => {
  if (doc.value.errors.length) return '请先修正 YAML 语法错误'
  if (hasAlias(doc.value, ['targets'])) return '服务器列表使用了 YAML 引用，请在 YAML 页添加'
  return ''
})

interface TargetCheck {
  runAsApps: string[]
  hasAppSecrets: boolean
  missingVars: Record<string, string[]>
  problems: string[]
}

const check = ref<TargetCheck>()
const checked = ref('')
const checking = ref(false)
const checkError = ref('')
const runAsApps = computed(() => check.value?.runAsApps ?? [])
const missingVars = computed(() => new Map(Object.entries(check.value?.missingVars ?? {})))
const varNames = computed(() => [...new Set([...vars.keys(), ...missingVars.value.keys()])].sort())
const reads = useReadRequests()

const draft = computed(() => {
  if (!visible.value || documentError.value) return
  // 表单还没填完时也检查应用依赖。临时标识和地址只用于预览，添加时必须用用户填写的值。
  let target = name.value && !nameError.value ? name.value : 'new-server'
  while ((!name.value || nameError.value) && targets.value.includes(target)) target += '-new'
  const entry: Record<string, unknown> = { host: form.host.trim() || 'localhost' }
  if (form.title.trim()) entry.title = form.title.trim()
  if (portOK.value && Number(form.port) !== 22) entry.port = Number(form.port)
  if (form.user.trim() && form.user.trim() !== 'root') entry.user = form.user.trim()
  if (form.auth === 'password') entry.auth = 'password'
  else if (form.identityFile.trim()) entry.identityFile = form.identityFile.trim()
  if (nonRoot.value && form.sudo) entry.sudo = true
  if (deploy.value) {
    if (form.root.trim()) entry.root = form.root.trim()
    if (form.runAs.trim() && !runAsInvalid.value) entry.runAs = form.runAs.trim()
    if (form.apps.length) entry.apps = [...form.apps]
    if (check.value?.hasAppSecrets && form.secretSource === 'file' && form.secretsFile.trim()) entry.secretsFrom = 'file:' + form.secretsFile.trim()
    const filled = [...vars].filter(([, value]) => value.trim()).map(([key, value]) => [key, value.trim()])
    if (filled.length) entry.vars = Object.fromEntries(filled)
  }
  const d = parse(props.text)
  addEntry(d, ['targets'], target, entry)
  return { name: target, text: serialize(props.text, d) }
})

// 只采用当前草稿的检查结果；慢回包不能让已经修改过的表单通过检查。
watch([() => draft.value?.name, () => draft.value?.text], ([name, text], _, onCleanup) => {
  checked.value = ''
  checkError.value = ''
  checking.value = !!text
  if (!name || !text) return
  const controller = new AbortController()
  const timer = setTimeout(async () => {
    try {
      const report = await reads.run('target', (signal) =>
        readCall<TargetCheck>(['validate', '--target', name, '-c', props.config, '--from-env'], signal, { FERRY_CONFIG_CONTENT: text }),
      { signal: controller.signal, timeoutMessage: '检查服务器配置超时' })
      if (controller.signal.aborted) return
      check.value = report
      checked.value = text
    } catch (e) {
      if (!controller.signal.aborted) checkError.value = (e as Error).message
    } finally {
      if (!controller.signal.aborted) checking.value = false
    }
  }, 250)
  onCleanup(() => {
    clearTimeout(timer)
    controller.abort()
    reads.cancel('target')
  })
}, { immediate: true })

const canAdd = computed(() => {
  const current = draft.value
  return !!current && checked.value === current.text && !checking.value && check.value
    && name.value && !nameError.value && form.host.trim() && portOK.value && !runAsInvalid.value
    && (!nonRoot.value || form.sudo) && (!runAsApps.value.length || form.runAs.trim())
    && (!check.value.hasAppSecrets || form.secretSource !== 'file' || form.secretsFile.trim())
    && !check.value.problems.length && missingVars.value.size === 0
})

function add() {
  if (!canAdd.value || !draft.value) return
  emit('add', draft.value.text, name.value)
}
</script>

<template>
  <el-dialog v-model="visible" title="新增服务器" width="680px" top="5vh" append-to-body :close-on-click-modal="false" @closed="emit('close')">
    <el-form label-width="118px" class="target-form" @submit.prevent="add">
      <el-form-item label="服务器标识" required :error="nameError"><el-input v-model="form.name" placeholder="如 prod、test-1" /></el-form-item>
      <el-form-item label="显示名"><el-input v-model="form.title" placeholder="留空时显示服务器标识" /></el-form-item>
      <el-form-item label="服务器地址" required><el-input v-model="form.host" placeholder="IP 或域名" /></el-form-item>
      <el-form-item label="SSH 端口" required :error="portOK ? '' : '请输入 1～65535 的端口'"><el-input v-model.trim="form.port" inputmode="numeric" /></el-form-item>
      <el-form-item label="SSH 用户"><el-input v-model="form.user" placeholder="默认 root" /></el-form-item>
      <el-form-item v-if="nonRoot" label="权限提升" :error="form.sudo ? '' : '非 root 登录时，部署需要免密 sudo'">
        <el-checkbox v-model="form.sudo">使用免密 sudo</el-checkbox>
      </el-form-item>
      <el-form-item label="登录方式">
        <el-select v-model="form.auth">
          <el-option label="私钥 / ssh-agent" value="key" />
          <el-option label="密码登录" value="password" />
        </el-select>
      </el-form-item>
      <el-form-item v-if="form.auth === 'key'" label="共享私钥路径">
        <div class="full">
          <el-input v-model="form.identityFile" placeholder="留空时用默认私钥、ssh-agent 或本机设置" />
          <div class="muted small">填写的路径会保存到 ferry.yaml，相对路径以配置文件所在目录为准。</div>
        </div>
      </el-form-item>
      <template v-if="deploy">
        <el-form-item label="部署根目录"><el-input v-model="form.root" :placeholder="rootDefault" /></el-form-item>
        <el-form-item label="可部署应用">
          <el-select v-model="form.apps" multiple collapse-tags collapse-tags-tooltip placeholder="默认允许全部应用">
            <el-option v-for="app in apps" :key="app" :value="app" :label="app" />
          </el-select>
        </el-form-item>
        <el-form-item label="运行用户" :required="!!runAsApps.length" :error="runAsInvalid ? '请填写非 root 的 Linux 用户' : ''">
          <div class="full">
            <el-input v-model="form.runAs" placeholder="如 ferry" />
            <div v-if="runAsApps.length" class="muted small">
              {{ runAsApps.slice(0, 3).join('、') }}{{ runAsApps.length > 3 ? `等 ${runAsApps.length} 个应用` : '' }}需要默认运行用户。用户不存在时，部署会创建。
            </div>
          </div>
        </el-form-item>
        <template v-if="check?.hasAppSecrets">
          <el-form-item label="应用密钥来源">
            <el-select v-model="form.secretSource">
              <el-option label="本机系统凭据库" value="keyring" />
              <el-option label="团队加密文件" value="file" />
            </el-select>
          </el-form-item>
          <el-form-item v-if="form.secretSource === 'file'" label="团队加密文件" required>
            <el-input v-model="form.secretsFile" placeholder="相对路径，如 secrets/prod.age" />
          </el-form-item>
        </template>
        <section v-if="varNames.length">
          <h4>服务器变量</h4>
          <p class="muted small">变量只对这台服务器的配置生效。供 envFile 使用的变量请填远端文件路径，密钥值留在文件内。</p>
          <el-form-item v-for="key in varNames" :key="key" :label="'${' + key + '}'" :required="missingVars.has(key)">
            <div class="full">
              <el-input :model-value="vars.get(key) ?? ''" @update:model-value="vars.set(key, $event)" />
              <div v-if="missingVars.has(key)" class="muted small" :title="missingVars.get(key)?.join('\n')">{{ missingVars.get(key)?.[0] }}</div>
            </div>
          </el-form-item>
        </section>
      </template>
      <el-alert v-if="documentError || checkError || check?.problems.length && !checking" type="error" :closable="false" show-icon>
        <div class="issues">{{ documentError || checkError || check?.problems.join('\n') }}</div>
      </el-alert>
    </el-form>
    <template #footer>
      <el-button @click="visible = false">取消</el-button>
      <el-button type="primary" :loading="checking" :disabled="!canAdd" @click="add">添加</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.target-form { max-height: calc(85dvh - 150px); overflow-y: auto; padding: 2px 12px 4px 0; }
.full { width: 100%; min-width: 0; }
.small { line-height: 1.5; overflow-wrap: anywhere; }
h4 { margin: 8px 0; }
.issues { white-space: pre-wrap; overflow-wrap: anywhere; }
</style>
