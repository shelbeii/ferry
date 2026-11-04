<script setup lang="ts">
// 服务器上的文件：点「文件」后从右边滑出。关掉再打开还在原来的目录。
import FilePanel from './FilePanel.vue'
import { ref, watch } from 'vue'
import { FullScreen } from '@element-plus/icons-vue'
import type { FileLocation } from '../fileLocations'

const props = defineProps<{ config: string; target: string; title: string; path?: string; locations?: FileLocation[] }>()
const open = defineModel<boolean>({ required: true })
const expanded = ref(false)
const visit = ref(0)
const selectedPath = ref(props.path)
watch(() => props.path, (path) => { selectedPath.value = path })
// 从软件 / 应用再次打开时回到指定位置；终端的文件抽屉仍保留上次浏览的目录。
watch([open, () => props.path], ([visible, path]) => {
  if (visible && path) { selectedPath.value = path; visit.value++ }
})
</script>

<template>
  <el-drawer v-model="open" direction="rtl" :size="expanded ? '94vw' : 'min(920px, 94vw)'" append-to-body :title="`文件：${title}`" class="files-drawer">
    <template #header><div class="files-drawer-heading"><strong class="el-drawer__title">文件 · {{ title }}</strong><el-button text size="small" :icon="FullScreen" :aria-pressed="expanded" @click="expanded = !expanded">{{ expanded ? '还原宽度' : '展开工作区' }}</el-button></div></template>
    <div v-if="locations && locations.length > 1" class="files-locations">
      <span>位置</span>
      <el-select v-model="selectedPath" aria-label="选择软件位置">
        <el-option v-for="location in locations" :key="location.path" :value="location.path" :label="`${location.label} · ${location.path}`" />
      </el-select>
    </div>
    <FilePanel :key="visit" :config="config" :target="target" :initial-path="selectedPath" />
  </el-drawer>
</template>

<style>
.files-locations { display: flex; align-items: center; gap: 10px; padding: 8px; }
.files-locations > span { flex-shrink: 0; color: var(--el-text-color-secondary); }
.files-locations .el-select { flex: 1; min-width: 0; }
.files-drawer-heading { display: flex; align-items: center; justify-content: center; flex-wrap: wrap; gap: 12px; width: 100%; min-width: 0; border: 0; background: transparent; }
.files-drawer-heading strong { flex: 0 0 100%; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.files-drawer .el-drawer__body {
  padding: 0;
  display: flex;
  flex-direction: column;
}
</style>
