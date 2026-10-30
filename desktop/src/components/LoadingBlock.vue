<script setup lang="ts">
import { Loading } from '@element-plus/icons-vue'

// 加载占位：先占住与加载完成后相近的高度（对话框不会先塌成一条再撑开），用骨架行示意，并说明正在做什么。
withDefaults(defineProps<{ text?: string; rows?: number; minHeight?: string }>(), {
  text: '正在加载…',
  rows: 5,
  minHeight: '240px',
})
</script>

<template>
  <div class="loading-block" :style="{ minHeight }">
    <div class="tip muted">
      <el-icon class="spin"><Loading /></el-icon>
      <span>{{ text }}</span>
    </div>
    <el-skeleton :rows="rows" animated />
  </div>
</template>

<style scoped>
.loading-block {
  display: flex;
  flex-direction: column;
  gap: 14px;
  padding: 4px 2px;
}
.tip {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 13px;
}
.spin {
  animation: spin 1s linear infinite;
}
@keyframes spin {
  to {
    transform: rotate(360deg);
  }
}
</style>
