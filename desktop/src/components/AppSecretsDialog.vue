<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { appSecretBlocksDeployment, appSecretStatus, call, type AppSecretStatus, type TeamSecretsStatus } from '../ferry'
import { startTask } from '../state'

const props = defineProps<{
  config: string
  target: string
  apps: string[]
  canPush?: boolean
  production?: boolean
  canRetry?: boolean
}>()
const emit = defineEmits<{ close: []; changed: [rows: AppSecretStatus[]]; pushed: []; retry: [] }>()
const visible = ref(true)
const loading = ref(false)
const saving = ref(false)
const error = ref('')
const rows = ref<AppSecretStatus[]>([])
const selection = ref<AppSecretStatus>()
const value = ref('')
const shared = ref(false)
const restart = ref(true)
const team = ref<TeamSecretsStatus>()
const teamExpanded = ref(false)
const newRecipient = ref('')
let disposed = false

const requiredMissing = computed(() => rows.value.some(appSecretBlocksDeployment))
const appArgs = () => [...props.apps, '-c', props.config]
const teamArgs = (...args: string[]) => ['secrets', props.target, ...args, '-c', props.config]

async function load() {
  loading.value = true
  error.value = ''
  try {
    const [values, status] = await Promise.all([
      call<AppSecretStatus[]>(['secrets', props.target, 'list', ...appArgs()]),
      call<TeamSecretsStatus>(teamArgs('team')),
    ])
    rows.value = values
    team.value = status
    if (status.path && !status.ready) teamExpanded.value = true
    emit('changed', rows.value)
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
}

function edit(row: AppSecretStatus) {
  selection.value = row
  value.value = ''
  shared.value = row.source === 'shared'
}

async function save(remove = false) {
  const row = selection.value
  if (!row || saving.value || (!remove && !value.value)) return
  if (remove) {
    const scope = team.value?.path ? `团队文件 ${team.value.path}` : shared.value ? '所有目标的默认值' : props.target
    try {
      await ElMessageBox.confirm(`删除 ${row.app} / ${row.name} 在「${scope}」的值？`, '删除应用密钥', { type: 'warning' })
    } catch {
      return
    }
  }
  const args = ['secrets', props.target, remove ? 'unset' : 'set', row.app, row.name, '-c', props.config]
  if (!team.value?.path && shared.value) args.push('--all-targets')
  saving.value = true
  error.value = ''
  try {
    await call(args, remove ? {} : { FERRY_SECRET_VALUE: value.value })
    value.value = ''
    selection.value = undefined
    await load()
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    saving.value = false
  }
}

async function teamAction(args: string[]) {
  if (saving.value) return
  saving.value = true
  error.value = ''
  try {
    await call(teamArgs(...args))
    if (args[1] === 'add') newRecipient.value = ''
    selection.value = undefined
    value.value = ''
    await load()
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    saving.value = false
  }
}

function addRecipient() {
  const key = newRecipient.value.trim()
  if (key) void teamAction(['team', 'add', key])
}

async function initTeam() {
  try {
    await ElMessageBox.confirm(`创建 ${team.value?.path}，并把本机公钥加入文件？已有团队的成员请先同步仓库。`, '初始化团队文件', { type: 'warning' })
  } catch { return }
  await teamAction(['team', 'init'])
}

async function copyPublicKey() {
  const key = team.value?.publicKey
  if (!key) return
  try {
    await navigator.clipboard.writeText(key)
    ElMessage.success('已复制公钥')
  } catch { ElMessage.warning('无法写入剪贴板，可选中公钥复制') }
}

async function removeRecipient(key: string) {
  try {
    await ElMessageBox.confirm('从当前文件移除此公钥？如需撤销对旧版本中密钥值的访问，请同时轮换密钥。', '移除成员', { type: 'warning' })
  } catch { return }
  await teamAction(['team', 'remove', key])
}

async function push() {
  const action = restart.value ? '更新密钥并重启服务' : '更新密钥'
  const message = restart.value
    ? `将密钥应用到 ${props.target} 的当前版本，并重启正在运行的服务。定时任务在下次运行时读取新值。`
    : `将密钥应用到 ${props.target} 的当前版本。服务在下次启动时读取新值。`
  try {
    if (props.production) {
      await ElMessageBox.prompt(`${message} 请输入生产目标 ${props.target} 确认。`, action, {
        inputValidator: (s) => s === props.target || '请输入完整的目标名称', type: 'warning',
      })
    } else {
      await ElMessageBox.confirm(message, action, { type: 'warning' })
    }
  } catch {
    return
  }
  const args = ['secrets', props.target, 'push', ...appArgs(), '--yes']
  if (!restart.value) args.push('--no-restart')
  startTask(`${props.target} · ${action}`, args, (task) => {
    if (task.status === 'ok' && !disposed) emit('pushed')
  })
}

onMounted(load)
onBeforeUnmount(() => { disposed = true; value.value = '' })
</script>

<template>
  <el-dialog v-model="visible" :title="`应用密钥（${target}）`" width="min(680px, 94vw)" append-to-body
    :close-on-click-modal="false" :before-close="(done: () => void) => { if (!saving) done() }" @closed="emit('close')">
    <el-alert v-if="error" :title="error" type="error" show-icon :closable="false" />
    <details v-if="team?.path" class="team-secrets" :open="teamExpanded"
      @toggle="teamExpanded = ($event.currentTarget as HTMLDetailsElement).open">
      <summary>团队共享 <code>{{ team.path }}</code></summary>
      <p v-if="team.problem" class="small">{{ team.problem }}</p>
      <div class="team-actions">
        <code v-if="team.publicKey" class="team-key" title="本机公钥">{{ team.publicKey }}</code>
        <el-button v-if="team.publicKey" size="small" @click="copyPublicKey">复制本机公钥</el-button>
        <el-button v-else size="small" :loading="saving" @click="teamAction(['keygen'])">生成本机公钥</el-button>
        <el-button v-if="team.publicKey && !team.exists" size="small" :loading="saving" @click="initTeam">初始化团队文件</el-button>
        <el-button v-if="!team.ready" size="small" :disabled="loading || saving" @click="load">重新读取</el-button>
      </div>
      <template v-if="team.ready">
        <p class="small muted">保存后将更新的加密文件提交到团队仓库。</p>
        <ul class="team-recipients" aria-label="收件人公钥">
          <li v-for="key in team.recipients" :key="key">
            <code>{{ key }}</code>
            <span v-if="key === team.publicKey" class="small muted">本机</span>
            <el-button v-else link size="small" type="danger" :disabled="saving" @click="removeRecipient(key)">移除</el-button>
          </li>
        </ul>
        <el-form class="team-actions" @submit.prevent="addRecipient">
          <el-input v-model="newRecipient" size="small" aria-label="成员的 age 公钥" placeholder="成员的 age1 公钥" :disabled="saving" />
          <el-button size="small" native-type="submit" :disabled="saving || !newRecipient.trim()">共享给成员</el-button>
        </el-form>
      </template>
    </details>
    <el-table v-loading="loading" :data="rows" empty-text="此目标没有声明应用密钥">
      <el-table-column v-if="apps.length !== 1" prop="app" label="应用" min-width="100" />
      <el-table-column prop="name" label="变量" min-width="180">
        <template #default="{ row }">
          <span class="mono">{{ row.name }}</span>
          <div v-if="row.doc" class="small muted">{{ row.doc }}</div>
        </template>
      </el-table-column>
      <el-table-column label="供值状态" width="140">
        <template #default="{ row }">
          <el-tag :type="row.available ? 'success' : row.optional ? 'info' : 'warning'" effect="plain">{{ appSecretStatus(row) }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column width="80" align="right">
        <template #default="{ row }">
          <el-button link type="primary" :disabled="loading || saving || (!!team?.path && !team.ready)" :aria-label="`填写 ${row.name}`" @click="edit(row)">填写</el-button>
        </template>
      </el-table-column>
    </el-table>
    <el-form v-if="selection" class="secret-form" label-position="top" @submit.prevent="save()">
      <el-form-item :label="`${selection.app} / ${selection.name}`">
        <el-input v-model="value" type="password" show-password autocomplete="new-password" :disabled="saving" />
      </el-form-item>
      <el-radio-group v-if="!team?.path" v-model="shared" :disabled="saving">
        <el-radio :value="false">仅 {{ target }}</el-radio>
        <el-radio :value="true">所有目标的默认值</el-radio>
      </el-radio-group>
      <p v-if="selection.source === 'env'" class="small muted">当前值由环境变量提供，优先于已保存的值。</p>
      <div class="secret-buttons">
        <el-button type="primary" native-type="submit" :loading="saving" :disabled="!value">保存密钥</el-button>
        <el-button :disabled="saving" @click="selection = undefined; value = ''">取消填写</el-button>
        <el-button link type="danger" :disabled="saving" @click="save(true)">删除该作用域的值</el-button>
      </div>
    </el-form>
    <template #footer>
      <div class="secret-footer">
        <el-checkbox v-if="canPush" v-model="restart">更新后重启服务</el-checkbox>
        <el-button v-if="canPush" type="primary" plain
          :disabled="loading || saving || !rows.length || requiredMissing || !!selection" @click="push">
          {{ restart ? '只更新密钥并重启' : '只更新密钥' }}
        </el-button>
        <el-button v-if="canRetry" type="primary" :disabled="loading || saving || !!error || requiredMissing || !!selection"
          @click="emit('retry'); visible = false">继续</el-button>
        <el-button :disabled="saving" @click="visible = false">关闭</el-button>
      </div>
    </template>
  </el-dialog>
</template>

<style scoped>
.secret-form { margin-top: 16px; padding: 16px; border: 1px solid var(--el-border-color-lighter); border-radius: 8px; }
.secret-buttons, .secret-footer { display: flex; flex-wrap: wrap; align-items: center; gap: 8px; }
.secret-buttons { margin-top: 12px; }
.secret-footer { justify-content: flex-end; }
.secret-footer .el-checkbox { margin-right: auto; }
.el-button + .el-button { margin-left: 0; }
.team-secrets { margin-bottom: 12px; }
.team-secrets summary { cursor: pointer; }
.team-actions, .team-recipients li { display: flex; align-items: center; flex-wrap: wrap; gap: 8px; margin-top: 8px; }
.team-actions .el-input { flex: 1 1 230px; }
.team-key, .team-recipients code { overflow-wrap: anywhere; }
.team-recipients { margin: 0; padding: 0; max-height: 160px; overflow-y: auto; }
</style>
