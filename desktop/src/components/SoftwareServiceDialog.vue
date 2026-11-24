<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { Delete, Plus } from '@element-plus/icons-vue'
import { call, type SwServiceEdit, type SwServiceView } from '../ferry'
import { startTask } from '../state'
import LoadingBlock from './LoadingBlock.vue'

const props = defineProps<{ config: string; target: string; instance: string }>()
const emit = defineEmits<{ close: []; saved: [] }>()

const loading = ref(true)
const loadError = ref('')
const view = ref<SwServiceView>()

const form = ref({
  exec: '',
  workdir: '',
  serviceType: 'simple' as 'simple' | 'forking',
  readyMode: 'tcp' as 'tcp' | 'http' | 'process',
  readyPort: 8080,
  readyUrl: '',
  readyTimeout: 180,
  logsText: '',
  envRows: [] as { key: string; value: string }[],
})
const validationError = ref('')

async function load() {
  loading.value = true
  loadError.value = ''
  try {
    const r = await call<SwServiceView>(['software', 'service', 'get', props.target, props.instance, '-c', props.config])
    view.value = r
    form.value = {
      exec: r.exec || '',
      workdir: r.workdir || r.dir || '',
      serviceType: r.serviceType === 'forking' ? 'forking' : 'simple',
      readyMode: r.readyMode || (r.readyPort ? 'tcp' : 'process'),
      readyPort: r.readyPort || 8080,
      readyUrl: r.readyUrl || '',
      readyTimeout: 180,
      logsText: (r.logs ?? []).join('\n'),
      envRows: Object.entries(r.env ?? {}).map(([key, value]) => ({ key, value })),
    }
  } catch (e) {
    loadError.value = (e as Error).message
  } finally {
    loading.value = false
  }
}

onMounted(load)

function addEnv() {
  form.value.envRows.push({ key: '', value: '' })
}
function removeEnv(i: number) {
  form.value.envRows.splice(i, 1)
}

function save() {
  validationError.value = ''
  const f = form.value
  const exec = f.exec.trim().replace(/\s*\n\s*/g, ' ')
  if (!exec) {
    validationError.value = '启动命令不能为空'
    return
  }
  if (f.readyMode === 'tcp' && (!f.readyPort || f.readyPort < 1 || f.readyPort > 65535)) {
    validationError.value = 'TCP 检测端口要在 1～65535 之间'
    return
  }
  if (f.readyMode === 'http' && !/^https?:\/\/(127\.0\.0\.1|localhost)/i.test(f.readyUrl.trim())) {
    validationError.value = 'HTTP 检查地址必须为本机 127.0.0.1 或 localhost'
    return
  }
  const env: Record<string, string> = {}
  for (const row of f.envRows) {
    const k = row.key.trim()
    if (!k && !row.value) continue
    if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(k)) {
      validationError.value = `环境变量名无效：${k || '（空）'}`
      return
    }
    env[k] = row.value
  }
  const logs = f.logsText
    .split(/\r?\n/)
    .map((l) => l.trim())
    .filter(Boolean)
  const payload: SwServiceEdit = {
    exec,
    workdir: f.workdir.trim() || undefined,
    serviceType: f.serviceType,
    readyMode: f.readyMode,
    readyPort: f.readyMode === 'process' ? 0 : f.readyPort,
    readyUrl: f.readyMode === 'http' ? f.readyUrl.trim() : undefined,
    readyTimeout: f.readyTimeout > 0 ? f.readyTimeout : 180,
    env: Object.keys(env).length ? env : undefined,
    logs: logs.length ? logs : undefined,
  }
  startTask(
    `更新 ${props.target} 上 ${props.instance} 的服务设置`,
    ['software', 'service', 'save', props.target, props.instance, '-c', props.config],
    (t) => {
      if (t.status === 'ok') {
        ElMessage.success(`${props.instance} 服务设置已更新并重启通过`)
        emit('saved')
      }
    },
    { FERRY_FILE_VALUE: JSON.stringify(payload) },
  )
  emit('close')
}
</script>

<template>
  <el-dialog :model-value="true" :title="`服务设置 · ${instance}`" width="760px" top="5vh" class="fit-dialog" append-to-body @close="emit('close')">
    <LoadingBlock v-if="loading" text="正在读取服务设置…" :rows="5" min-height="220px" />
    <el-alert v-else-if="loadError" type="error" :title="loadError" :closable="false" show-icon />
    <template v-else-if="view">
      <p class="muted small desc">
        系统服务 <span class="mono">{{ view.unit }}</span> · 运行用户 <span class="mono">{{ view.user }}</span> · 安装目录 <span class="mono">{{ view.dir }}</span>。
        保存后会先校验单元并重启验证，如果启动失败会自动回滚到修改前的配置。
      </p>
      <el-alert v-if="validationError" type="error" :title="validationError" :closable="false" show-icon class="mb" />
      <el-form label-width="110px">
        <el-form-item label="启动命令">
          <el-input v-model="form.exec" type="textarea" :rows="3" class="mono" />
        </el-form-item>
        <el-form-item label="工作目录">
          <el-input v-model="form.workdir" class="mono" :placeholder="view.dir" />
        </el-form-item>
        <el-form-item label="启动方式">
          <el-radio-group v-model="form.serviceType">
            <el-radio value="simple">前台运行（simple，推荐）</el-radio>
            <el-radio value="forking">后台守护进程（forking）</el-radio>
          </el-radio-group>
        </el-form-item>
        <el-form-item label="就绪检查">
          <el-radio-group v-model="form.readyMode">
            <el-radio value="tcp">TCP 端口</el-radio>
            <el-radio value="http">HTTP 接口</el-radio>
            <el-radio value="process">仅检查进程存活</el-radio>
          </el-radio-group>
        </el-form-item>
        <el-form-item v-if="form.readyMode !== 'process'" label="检测端口">
          <el-input-number v-model="form.readyPort" :min="1" :max="65535" :controls="false" class="port" />
          <span class="muted small ml">超时（秒）</span>
          <el-input-number v-model="form.readyTimeout" :min="15" :max="600" size="small" class="ml" />
        </el-form-item>
        <el-form-item v-if="form.readyMode === 'http'" label="检查地址">
          <el-input v-model="form.readyUrl" class="mono" placeholder="http://127.0.0.1:8080/health" />
        </el-form-item>
        <el-form-item label="环境变量">
          <div class="env-list">
            <div v-for="(row, i) in form.envRows" :key="i" class="env-row">
              <el-input v-model="row.key" class="mono env-k" placeholder="变量名（如 APP_ENV）" />
              <span class="muted">=</span>
              <el-input v-model="row.value" class="mono env-v" placeholder="值" />
              <el-button link :icon="Delete" @click="removeEnv(i)" />
            </div>
            <el-button link type="primary" size="small" :icon="Plus" @click="addEnv">添加环境变量</el-button>
          </div>
        </el-form-item>
        <el-form-item label="日志文件">
          <el-input v-model="form.logsText" type="textarea" :rows="2" class="mono" :placeholder="`${view?.dir ?? ''}/logs/app.log（每行一个，须在安装目录内）`" />
        </el-form-item>
      </el-form>
    </template>
    <template #footer>
      <el-button @click="emit('close')">取消</el-button>
      <el-button v-if="!loading && !loadError" type="primary" @click="save">保存并重启验证</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.desc { margin: 0 0 12px; line-height: 1.6; }
.mb { margin-bottom: 10px; }
.port { width: 120px; }
.ml { margin-left: 10px; }
.env-list { display: flex; flex-direction: column; gap: 6px; width: 100%; }
.env-row { display: flex; align-items: center; gap: 6px; }
.env-k { flex: 0 0 220px; }
.env-v { flex: 1; }
.small { font-size: 12px; }
</style>
