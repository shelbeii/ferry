<script setup lang="ts">
import { computed } from 'vue'
import { ArrowDown } from '@element-plus/icons-vue'
import type { RowAction } from '../rowActions'

const props = withDefaults(defineProps<{ actions: RowAction[]; label: string; inlineCount?: number }>(), { inlineCount: 2 })
const inline = computed(() => props.actions.slice(0, props.inlineCount))
const more = computed(() => props.actions.slice(props.inlineCount))
</script>

<template>
  <div class="row-actions" @click.stop>
    <el-button v-for="action in inline" :key="action.id" link :type="action.danger ? 'danger' : 'primary'" size="small" :disabled="action.disabled" :title="action.title" @click="action.run">{{ action.label }}</el-button>
    <el-dropdown v-if="more.length" trigger="click" placement="bottom-end">
      <el-button link type="primary" size="small" :aria-label="`${label}的更多操作`">更多<el-icon class="more-arrow"><ArrowDown /></el-icon></el-button>
      <template #dropdown>
        <el-dropdown-menu>
          <el-dropdown-item v-for="(action, index) in more" :key="action.id" :disabled="action.disabled" :title="action.title" :divided="action.danger && !more[index - 1]?.danger" @click="action.run">
            <span :class="{ danger: action.danger }">{{ action.label }}</span>
          </el-dropdown-item>
        </el-dropdown-menu>
      </template>
    </el-dropdown>
  </div>
</template>

<style scoped>
.row-actions { display: inline-flex; align-items: center; gap: 12px; flex-wrap: nowrap; white-space: nowrap; }
.row-actions .el-button { height: 24px; margin: 0; padding: 0; }
.more-arrow { margin-left: 3px; }
.danger { color: var(--el-color-danger); }
</style>
