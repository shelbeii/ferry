<script setup lang="ts">
// 服务器上的文件：点「文件」后从右边滑出。关掉再打开还在原来的目录。
import FilePanel from './FilePanel.vue'
import { ref } from 'vue'
import { FullScreen } from '@element-plus/icons-vue'

defineProps<{ config: string; target: string; title: string }>()
const open = defineModel<boolean>({ required: true })
const expanded = ref(false)
</script>

<template>
  <el-drawer v-model="open" direction="rtl" :size="expanded ? '94vw' : 'min(920px, 94vw)'" append-to-body :title="`文件：${title}`" class="files-drawer">
    <template #header><div class="files-drawer-heading"><strong class="el-drawer__title">文件 · {{ title }}</strong><el-button text size="small" :icon="FullScreen" :aria-pressed="expanded" @click="expanded = !expanded">{{ expanded ? '还原宽度' : '展开工作区' }}</el-button></div></template>
    <FilePanel :config="config" :target="target" />
  </el-drawer>
</template>

<style>
.files-drawer-heading { display: flex; align-items: center; justify-content: center; flex-wrap: wrap; gap: 12px; width: 100%; min-width: 0; border: 0; background: transparent; }
.files-drawer-heading strong { flex: 0 0 100%; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.files-drawer .el-drawer__body {
  padding: 0;
  display: flex;
  flex-direction: column;
}
</style>
