<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { Close } from '@element-plus/icons-vue'
import { bytes, ferry, type FileEntry, type RemoteFileSize, type Running } from '../ferry'
import { HOVER_DELAY } from '../ui'

const props = defineProps<{ config: string; target: string; path: string; entry: FileEntry; disabled?: boolean }>()
const loading = ref(false)
const result = ref<RemoteFileSize>()
const error = ref('')
let request: Running<RemoteFileSize> | undefined
let sequence = 0
function cancel() {
  sequence++
  void request?.kill()
  request = undefined
  loading.value = false
}
function reset() { cancel(); result.value = undefined; error.value = '' }
watch(() => [props.config, props.target, props.path, props.entry.mtime, props.entry.size], reset)
onBeforeUnmount(cancel)
const hint = computed(() => error.value || (result.value
  ? `${result.value.bytes.toLocaleString()} 字节；点击重新计算${result.value.dir ? '。目录递归统计，包含目录项，内部链接不展开' : ''}`
  : props.entry.dir ? '点击递归统计大小；内部符号链接不展开' : `${props.entry.size.toLocaleString()} 字节；点击重新读取大小`))
async function measure() {
  if (props.disabled || loading.value) return
  const token = ++sequence
  loading.value = true
  error.value = ''
  const proc = ferry<RemoteFileSize>(['files', 'size', props.target, props.path, '-c', props.config])
  request = proc
  try {
    const response = await proc.done
    if (token !== sequence) return
    if (!response.ok || !response.data) throw new Error(response.error || '大小统计失败')
    result.value = response.data
  } catch (e) {
    if (token === sequence) error.value = (e as Error).message
  } finally {
    if (token === sequence) { loading.value = false; request = undefined }
  }
}
</script>

<template>
  <span class="file-size-cell" :aria-busy="loading" aria-live="polite" @click.stop @dblclick.stop>
    <el-tooltip :content="hint" :show-after="HOVER_DELAY" placement="left">
      <el-button link :type="error ? 'danger' : 'primary'" size="small" :disabled="disabled || loading" :loading="loading" :aria-label="`${error ? '重新统计' : '查看'}${entry.name}的大小`" @click="measure">
        {{ loading ? '计算中' : error ? '重试' : result ? bytes(result.bytes) : entry.dir ? '查看大小' : bytes(entry.size) }}
      </el-button>
    </el-tooltip>
    <el-button v-if="loading" link size="small" :icon="Close" :aria-label="`取消统计${entry.name}的大小`" title="取消统计" @click="cancel" />
  </span>
</template>

<style scoped>
.file-size-cell { display: inline-flex; align-items: center; justify-content: flex-end; gap: 4px; max-width: 100%; }
.file-size-cell .el-button { margin: 0; font-size: 13px; font-variant-numeric: tabular-nums; }
</style>
