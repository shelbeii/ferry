<script setup lang="ts">
import { nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { Loading, Refresh } from '@element-plus/icons-vue'
import { ferry, type Running } from '../ferry'

// name：Ferry 装的软件的实例名；不是 Ferry 装的给 unit（系统服务）或 container（容器），name 只用来显示
const props = defineProps<{ config: string; target: string; name: string; unit?: string; container?: string }>()
const other = !!(props.unit || props.container)
const emit = defineEmits<{ close: [] }>()

const source = ref<'journal' | 'main'>('journal')
const lines = ref<string[]>([])
const from = ref('')
const error = ref('')
const loading = ref(false)
const box = ref<HTMLElement>()
// 等了几秒（大日志、慢网络时要等一会儿，说清楚在读，免得以为没有日志）
const waited = ref(0)
let waitTimer: ReturnType<typeof setInterval> | undefined
let serial = 0
let request: Running<{ from: string; lines: string[] | null }> | undefined

async function load() {
  const seq = ++serial
  request?.kill()
  loading.value = true
  error.value = ''
  waited.value = 0
  clearInterval(waitTimer)
  waitTimer = setInterval(() => waited.value++, 1000)
  try {
    const args = other
      ? ['software', 'logs', props.target, ...(props.unit ? ['--unit', props.unit] : ['--container', props.container!]), '-n', '500', '-c', props.config]
      : ['software', 'logs', props.target, props.name, '-n', '500', ...(source.value === 'main' ? ['--main-log'] : []), '-c', props.config]
    const proc = ferry<{ from: string; lines: string[] | null }>(args)
    request = proc
    const result = await proc.done
    if (seq !== serial) return
    if (!result.ok || !result.data) throw new Error(result.error ?? '日志读取失败')
    const r = result.data
    lines.value = r.lines ?? []
    from.value = r.from
    await nextTick()
    box.value?.scrollTo({ top: box.value.scrollHeight })
  } catch (e) {
    if (seq !== serial) return
    lines.value = []
    error.value = (e as Error).message
  } finally {
    if (seq === serial) {
      loading.value = false
      clearInterval(waitTimer)
    }
  }
}
onMounted(load)
onBeforeUnmount(() => { serial++; request?.kill(); clearInterval(waitTimer) })
watch(source, load)
</script>

<template>
  <el-dialog :model-value="true" append-to-body :title="`${name} 的日志（${target}）`" width="900px" top="4vh" @close="emit('close')">
    <div class="bar">
      <el-radio-group v-if="!other" v-model="source" size="small">
        <el-radio-button value="journal">系统服务输出</el-radio-button>
        <el-radio-button value="main">软件自己的日志</el-radio-button>
      </el-radio-group>
      <span class="muted mono small">{{ from }}</span>
      <span class="spacer" />
      <el-button :icon="Refresh" :loading="loading" size="small" circle @click="load" />
    </div>
    <el-alert v-if="error" type="error" :title="error" :closable="false" show-icon />
    <div class="wrap">
      <pre ref="box" class="log mono">{{ lines.length ? lines.join('\n') : loading ? '' : '（没有内容）' }}</pre>
      <!-- 读取中：没有内容时居中显示；重新读取时盖一层在旧内容上 -->
      <div v-if="loading" class="loading" :class="{ over: lines.length }">
        <el-icon class="is-loading spin"><Loading /></el-icon>
        <div>正在读取{{ source === 'main' && !other ? '软件自己的日志' : '日志' }}的最后 500 行…</div>
        <div v-if="waited >= 2" class="sub">已等 {{ waited }} 秒<template v-if="waited >= 8">：网络较慢或服务器忙，还在读</template></div>
      </div>
    </div>
  </el-dialog>
</template>

<style scoped>
.bar {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-bottom: 8px;
  flex-wrap: wrap;
}
.bar .mono { min-width: 0; overflow-wrap: anywhere; }
.spacer {
  flex: 1;
}
.small {
  font-size: 12px;
}
.wrap {
  position: relative;
}
.loading {
  position: absolute;
  inset: 0;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 10px;
  font-size: 13px;
  color: var(--el-text-color-regular);
  border-radius: 4px;
}
.loading.over {
  background: color-mix(in srgb, var(--el-bg-color) 70%, transparent);
}
.spin {
  font-size: 28px;
  color: var(--el-color-primary);
}
.sub {
  font-size: 12px;
  color: var(--el-text-color-secondary);
}
.log {
  height: min(460px, calc(100dvh - 260px));
  min-height: 160px;
  box-sizing: border-box;
  overflow: auto;
  margin: 0;
  padding: 10px;
  background: var(--el-fill-color-lighter);
  border-radius: 4px;
  font-size: 12px;
  white-space: pre-wrap;
  word-break: break-all;
}
</style>
