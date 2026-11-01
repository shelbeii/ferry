<script setup lang="ts">
// 服务器告警（ferry monitor）：服务器上每隔几分钟检查一次，有问题时发到设置 → 通知里的地方。可以随时打开、关闭。
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { call, NOTIFY_KINDS } from '../ferry'
import { appSettings, settingsRequest } from '../state'
import LoadingBlock from './LoadingBlock.vue'
import GlassToggle from './glass/GlassToggle.vue'

const props = defineProps<{ config: string; target: string }>()
const emit = defineEmits<{ close: [] }>()
const visible = ref(true)

interface Problem {
  key: string
  level: 'bad' | 'warn'
  text: string
  since?: string
}
interface Status {
  on: boolean
  intervalMin?: number
  diskPct?: number
  certDays?: number
  channels?: string[]
  lastRun?: string
  problems: Problem[]
  sent?: Record<string, string>
}

const status = ref<Status>()
const error = ref('')
const busy = ref('')
const checked = ref<Problem[]>()
const form = reactive({ channels: [] as string[], interval: 5, disk: 90, certDays: 14 })
const channels = computed(() => (appSettings.value?.notify?.channels ?? []).filter((c) => !c.off))

function setChannel(id: string, enabled: boolean) {
  form.channels = enabled ? [...new Set([...form.channels, id])] : form.channels.filter((c) => c !== id)
}

async function run<T>(action: string, extra: string[] = []): Promise<T | undefined> {
  busy.value = action
  error.value = ''
  try {
    return await call<T>(['monitor', props.target, action, ...extra, '-c', props.config])
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    busy.value = ''
  }
}

async function load() {
  const s = await run<Status>('status')
  if (!s) return
  status.value = s
  if (s.on) {
    form.interval = s.intervalMin ?? 5
    form.disk = s.diskPct ?? 90
    form.certDays = s.certDays ?? 14
    form.channels = channels.value.filter((c) => s.channels?.includes(c.name)).map((c) => c.id)
  } else {
    form.channels = channels.value.map((c) => c.id)
  }
}
onMounted(load)

async function turnOn() {
  if (!form.channels.length) {
    ElMessage.warning('选一个告警发到的地方')
    return
  }
  const s = await run<Status>('on', ['--notify', form.channels.join(','), '--interval', String(form.interval), '--disk', String(form.disk), '--cert-days', String(form.certDays)])
  if (s) {
    ElMessage.success(status.value?.on ? '设置已保存' : '告警已打开')
    await load()
  }
}

async function turnOff() {
  if (await run('off')) {
    ElMessage.success('告警已关闭')
    await load()
  }
}

async function checkNow() {
  const s = await run<Status>('check')
  if (s) checked.value = s.problems
}

async function test() {
  const s = await run<Status>('test')
  if (!s) return
  const bad = Object.entries(s.sent ?? {}).filter(([, e]) => e)
  if (bad.length) ElMessage.error(bad.map(([n, e]) => `${n}：${e}`).join('；'))
  else ElMessage.success('测试告警已发出，去群里看看')
}

const when = (t?: string) => (t ? new Date(t).toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' }) : '')
</script>

<template>
  <el-dialog v-model="visible" :title="`服务器告警（${target}）`" width="600px" append-to-body @closed="emit('close')">
    <LoadingBlock v-if="!status && !error" :text="`正在连接 ${target}…`" :rows="4" min-height="200px" />
    <el-alert v-if="error" type="error" :title="error" :closable="false" show-icon class="block" />
    <template v-if="status">
      <p class="muted small">
        服务器上的 Ferry 每隔几分钟检查一次：应用停了或反复重启、不是部署引起的重启、磁盘快满、HTTPS 证书快到期、防火墙被关、新出现对外开放的端口。
        问题出现时、恢复时各发一次通知；这台电脑不用开着。
      </p>
      <div class="state">
        <el-tag :type="status.on ? 'success' : 'info'" effect="plain">{{ status.on ? '开着' : '没开' }}</el-tag>
        <span v-if="status.on" class="muted small">每 {{ status.intervalMin }} 分钟检查一次，发到 {{ status.channels?.join('、') }}</span>
        <span v-if="status.lastRun" class="muted small">· 上次检查 {{ when(status.lastRun) }}</span>
      </div>
      <div v-if="status.on" class="problems">
        <div v-if="!status.problems.length" class="ok small">现在没有问题</div>
        <div v-for="p in status.problems" :key="p.key" class="small" :class="p.level">{{ p.text }}<span class="muted">（{{ when(p.since) }} 起）</span></div>
      </div>

      <h4>设置</h4>
      <el-form label-width="96px">
        <el-form-item label="发到">
          <div v-if="!channels.length" class="muted small">
            还没有地方发告警：先在<el-button link type="primary" @click="settingsRequest = 'notify'">设置 → 通知</el-button>里添加群机器人
          </div>
          <div v-else class="channel-list" role="group" aria-label="告警接收渠道">
            <GlassToggle v-for="c in channels" :key="c.id" :model-value="form.channels.includes(c.id)" size="default" :disabled="!!busy" @update:model-value="(enabled: boolean) => setChannel(c.id, enabled)">{{ c.name }}（{{ NOTIFY_KINDS[c.kind] }}）</GlassToggle>
          </div>
        </el-form-item>
        <!-- 三个数字框左边对齐，单位和说明都放在后面 -->
        <el-form-item label="检查间隔">
          <el-input-number v-model="form.interval" :min="1" :max="1440" size="small" class="num" />
          <span class="muted small ml">分钟检查一次</span>
        </el-form-item>
        <el-form-item label="磁盘">
          <el-input-number v-model="form.disk" :min="50" :max="99" size="small" class="num" />
          <span class="muted small ml">%，用到这么多时告警</span>
        </el-form-item>
        <el-form-item label="HTTPS 证书">
          <el-input-number v-model="form.certDays" :min="1" :max="60" size="small" class="num" />
          <span class="muted small ml">天，剩这么多天到期时告警</span>
        </el-form-item>
      </el-form>
      <div v-if="checked" class="problems">
        <div class="small muted">现在检查的结果：</div>
        <div v-if="!checked.length" class="ok small">没有发现问题</div>
        <div v-for="p in checked" :key="p.key + p.text" class="small" :class="p.level">{{ p.text }}</div>
      </div>
    </template>
    <template #footer>
      <el-button :loading="busy === 'check'" :disabled="!status" @click="checkNow">现在检查一次</el-button>
      <el-button v-if="status?.on" :loading="busy === 'test'" @click="test">发测试告警</el-button>
      <el-button v-if="status?.on" type="danger" plain :loading="busy === 'off'" @click="turnOff">关闭告警</el-button>
      <el-button type="primary" :loading="busy === 'on'" :disabled="!status || !channels.length" @click="turnOn">{{ status?.on ? '保存设置' : '打开告警' }}</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.small {
  font-size: 12px;
}
.ml {
  margin-left: 8px;
}
.num {
  width: 130px;
}
.channel-list {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: 12px;
  padding-block: 3px;
}
.block {
  margin-bottom: 10px;
}
.state {
  display: flex;
  align-items: center;
  gap: 8px;
}
.problems {
  margin: 8px 0;
  line-height: 1.8;
}
.ok {
  color: var(--el-color-success);
}
.bad {
  color: var(--el-color-danger);
}
.warn {
  color: var(--el-color-warning);
}
</style>
