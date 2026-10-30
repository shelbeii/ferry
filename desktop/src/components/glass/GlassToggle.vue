<script setup lang="ts">
// 带文字的玻璃开关，替代「勾选框 + 文字」：开关在前、文字在后，点文字也能切换。
// 用法和 el-checkbox 一样：v-model，或 :model-value + @update:model-value；文字写在默认插槽里。
import { useId } from 'vue'
import GlassSwitch from './GlassSwitch.vue'

const model = defineModel<boolean>({ required: true })
const props = withDefaults(defineProps<{ disabled?: boolean; size?: 'default' | 'small' }>(), { disabled: false, size: 'small' })
const id = `cg-toggle-${useId().replace(/[^\w-]/g, '')}`
function flip() { if (!props.disabled) model.value = !model.value }
</script>

<template>
  <span class="cg-toggle" :class="{ 'is-disabled': disabled }">
    <GlassSwitch v-model="model" :size="size" :disabled="disabled" :aria-labelledby="id" />
    <span :id="id" class="cg-toggle-text" @click="flip"><slot /></span>
  </span>
</template>

<style>
.cg-toggle { display: inline-flex; align-items: center; gap: 10px; min-height: 30px; vertical-align: middle; }
.cg-toggle-text { font-size: 14px; line-height: 1.5; color: var(--el-text-color-regular); cursor: pointer; user-select: none; }
.cg-toggle.is-disabled .cg-toggle-text { color: var(--el-text-color-placeholder); cursor: not-allowed; }
</style>
