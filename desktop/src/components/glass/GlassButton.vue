<script setup lang="ts">
import { onMounted, type Component } from 'vue'
import { Loading } from '@element-plus/icons-vue'
import { installGlassButtons } from '../../glass/buttons'
import GlassPane from './GlassPane.vue'

defineProps<{ icon?: Component; disabled?: boolean; loading?: boolean; size?: 'small'; tone?: 'primary' | 'danger' }>()

onMounted(installGlassButtons)
</script>

<template>
  <GlassPane as="button" type="button" variant="clear" radius="pill" class="cg-button"
    :adaptive="!tone" :class="[tone && `cg-button--${tone}`, { 'cg-icon-only': !$slots.default && !$slots.content, 'cg-button-small': size === 'small' }]"
    :disabled="disabled || loading" :aria-busy="loading || undefined">
    <span class="cg-layer cg-glow" aria-hidden="true" />
    <el-icon v-if="loading" class="is-loading"><Loading /></el-icon>
    <el-icon v-else-if="icon"><component :is="icon" /></el-icon>
    <slot v-if="$slots.content" name="content" />
    <span v-else-if="$slots.default" class="cg-button-label"><slot /></span>
  </GlassPane>
</template>

<style>
.cg-button {
  --cg-button-height: 36px;
  display: inline-flex; align-items: center; justify-content: center; gap: 6px;
  height: var(--cg-button-height); padding: 0 15px; box-sizing: border-box; flex-shrink: 0;
  border: 0; background: none; font: inherit; font-size: 13px; font-weight: 500;
  cursor: pointer; -webkit-tap-highlight-color: transparent; user-select: none;
}
.cg-button-small { --cg-button-height: 28px; padding: 0 11px; font-size: 12px; }
.cg-button.cg-icon-only { width: var(--cg-button-height); padding: 0; }
.cg-button > .el-icon { font-size: 16px; }
.cg-button-small > .el-icon { font-size: 14px; }
.el-dialog .cg-button { max-width: 100%; min-height: var(--cg-button-height); height: auto; padding-block: 4px; line-height: 1.4; }
.el-dialog .cg-button > .el-icon { flex: none; }
.el-dialog .cg-button-label { min-width: 0; overflow-wrap: anywhere; }
.cg-button:focus-visible { outline: 2px solid var(--el-color-primary); outline-offset: 3px; }
.cg-button:disabled { cursor: not-allowed; opacity: .55; }
.cg-button.cg-plain:where(:not(.cg-button--primary, .cg-button--danger)):hover:not(:disabled) { color: var(--el-color-primary); }
.cg-button.cg-plain:where(:not(.cg-button--primary, .cg-button--danger)):hover:not(:disabled) > .cg-tint { box-shadow: inset 0 0 0 1px var(--el-color-primary-light-5); background: var(--el-color-primary-light-9); }
.cg-button--primary { --cg-button-accent: var(--el-color-primary); --cg-button-fill: var(--el-color-primary-light-9); }
.cg-button--danger { --cg-button-accent: var(--el-color-danger); --cg-button-fill: var(--el-color-danger-light-9); }
.cg-button.cg-plain:is(.cg-button--primary, .cg-button--danger) {
  color: color-mix(in srgb, var(--cg-button-accent) 60%, var(--el-text-color-primary));
}
.cg.cg-button.cg-plain:is(.cg-button--primary, .cg-button--danger) > .cg-tint {
  background: var(--cg-button-fill);
  box-shadow: inset 0 0 0 1px color-mix(in srgb, var(--cg-button-accent) 45%, transparent);
}
.cg.cg-button.cg-plain:is(.cg-button--primary, .cg-button--danger):hover:not(:disabled) > .cg-tint {
  box-shadow: inset 0 0 0 1px var(--cg-button-accent);
}
</style>
