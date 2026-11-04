<script setup lang="ts">
import { computed, ref } from 'vue'
import FilePanel from './FilePanel.vue'

const props = defineProps<{ config: string; target: string; path: string; validate?: (path: string) => string }>()
const emit = defineEmits<{ select: [path: string]; close: [] }>()
const selected = ref('')
// 在父目录中定位当前值：安装位置尚未创建时，也可以浏览父目录并新建它。
const initial = computed(() => {
  const path = props.path.trim().replace(/\/+$/, '')
  if (!path.startsWith('/')) return { dir: '/', name: undefined }
  const split = path.lastIndexOf('/')
  return { dir: path.slice(0, split) || '/', name: path.slice(split + 1) || undefined }
})
const error = computed(() => selected.value ? props.validate?.(selected.value) || '' : '')

function select() {
  if (selected.value && !error.value) emit('select', selected.value)
}
</script>

<template>
  <el-dialog :model-value="true" :title="`选择服务器目录 · ${target}`" width="min(960px, 94vw)" top="5vh" append-to-body :close-on-click-modal="false" class="remote-directory-dialog" @close="emit('close')">
    <div class="directory-browser">
      <FilePanel :config="config" :target="target" :initial-path="initial.dir" :initial-focus="initial.name" directory-only @directory-change="selected = $event" />
    </div>
    <div class="directory-selection"><span class="muted">已选目录</span><span class="mono">{{ selected || '请先打开可访问的服务器目录' }}</span></div>
    <el-alert v-if="error" :title="error" type="warning" :closable="false" show-icon />
    <template #footer>
      <el-button @click="emit('close')">取消</el-button>
      <el-button type="primary" :disabled="!selected || !!error" @click="select">选择此目录</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.directory-browser { height: min(54vh, 520px); min-height: 240px; border: 1px solid var(--el-border-color-lighter); border-radius: 8px; overflow: hidden; }
.directory-selection { display: flex; align-items: baseline; gap: 12px; padding: 12px 0; font-size: 14px; }
.directory-selection > :first-child { flex-shrink: 0; }
.directory-selection .mono { min-width: 0; overflow-wrap: anywhere; }
</style>
