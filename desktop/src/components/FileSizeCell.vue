<script setup lang="ts">
// 文件列表的「大小」格：结果和统计过程放在面板里（fileSizes.ts），这里只显示；悬停说明由列表共用的提示显示（data-tip）。
import { computed } from 'vue'
import { Close } from '@element-plus/icons-vue'
import { bytes, type FileEntry } from '../ferry'
import type { SizeState } from '../fileSizes'

const props = defineProps<{ entry: FileEntry; state?: SizeState; disabled?: boolean }>()
const emit = defineEmits<{ measure: []; cancel: [] }>()
const loading = computed(() => !!props.state?.loading)
const result = computed(() => props.state?.result)
const error = computed(() => props.state?.error ?? '')
const hint = computed(() => error.value || (result.value
  ? `${result.value.bytes.toLocaleString()} 字节；点击重新计算${result.value.dir ? '。目录递归统计，包含目录项，内部链接不展开' : ''}`
  : props.entry.dir ? '点击递归统计大小；内部符号链接不展开' : `${props.entry.size.toLocaleString()} 字节；点击重新读取大小`))
function measure() {
  if (!props.disabled && !loading.value) emit('measure')
}
</script>

<template>
  <span class="file-size-cell" :aria-busy="loading" aria-live="polite" @click.stop @dblclick.stop>
    <el-button link :type="error ? 'danger' : 'primary'" size="small" :disabled="disabled || loading" :loading="loading" data-tip="size" :data-tip-text="hint" :aria-label="`${error ? '重新统计' : '查看'}${entry.name}的大小`" @click="measure">
      {{ loading ? '计算中' : error ? '重试' : result ? bytes(result.bytes) : entry.dir ? '查看大小' : bytes(entry.size) }}
    </el-button>
    <el-button v-if="loading" link size="small" :icon="Close" :aria-label="`取消统计${entry.name}的大小`" title="取消统计" @click="emit('cancel')" />
  </span>
</template>

<style scoped>
.file-size-cell { display: inline-flex; align-items: center; justify-content: flex-end; gap: 4px; max-width: 100%; }
.file-size-cell .el-button { margin: 0; font-size: 13px; font-variant-numeric: tabular-nums; }
</style>
