<script setup lang="ts">
import { computed } from 'vue'
import { filePermissions } from '../filePermissions'
import { HOVER_DELAY } from '../ui'

const props = defineProps<{ mode: string; dir: boolean; link?: boolean }>()
const permissions = computed(() => filePermissions(props))
</script>

<template>
  <el-tooltip :show-after="HOVER_DELAY" placement="left" effect="light">
    <template #content>
      <div class="permission-details">
        <div class="permission-heading"><strong>权限详情</strong><span v-if="permissions.octal" class="mono">{{ permissions.octal }}</span></div>
        <div v-for="item in permissions.roles" :key="item.role" class="permission-detail-row"><span>{{ item.role }}</span><strong>{{ item.text }}</strong></div>
        <p v-if="permissions.roles.length" class="permission-note">所有者是文件所属用户，不一定是当前登录用户。</p>
        <p v-if="dir && permissions.roles.length" class="permission-note">读取：列出名称；写入：创建、删除或改名（还需有“进入”权限）；进入：访问文件夹内的条目。</p>
        <p v-if="permissions.note" class="permission-note">{{ permissions.note }}</p>
        <p v-for="text in permissions.special" :key="text" class="permission-note">{{ text }}</p>
        <p v-if="permissions.roles.length" class="permission-note">实际访问还受上级目录和访问控制规则影响。</p>
        <div class="permission-raw">原始权限 <code>{{ mode || '未提供' }}</code></div>
      </div>
    </template>
    <span class="permission-summary" tabindex="0" @click.stop @dblclick.stop>
      <span v-for="item in permissions.summary" :key="item.role" class="permission-line"><span v-if="item.role" class="permission-role">{{ item.role }}</span><span>{{ item.text }}</span></span>
      <span v-if="permissions.special.length" class="permission-special">含特殊权限</span>
    </span>
  </el-tooltip>
</template>

<style scoped>
.permission-summary { display: inline-flex; flex-direction: column; gap: 2px; max-width: 100%; padding-block: 2px; border-radius: 4px; color: var(--el-text-color-regular); cursor: help; font-size: 12px; }
.permission-summary:focus-visible { outline-offset: 1px; }
.permission-line { display: flex; flex-wrap: wrap; column-gap: 8px; line-height: 1.6; }
.permission-role { color: var(--el-text-color-secondary); }
.permission-special { color: var(--el-text-color-secondary); font-size: 11px; }
.permission-details { width: 260px; max-width: calc(100vw - 64px); font-size: 12px; line-height: 1.7; color: var(--el-text-color-primary); }
.permission-heading, .permission-detail-row { display: flex; align-items: center; justify-content: space-between; gap: 16px; }
.permission-heading { margin-bottom: 8px; }
.permission-heading > span { color: var(--el-text-color-secondary); }
.permission-detail-row > span { color: var(--el-text-color-secondary); }
.permission-detail-row strong { font-weight: 500; }
.permission-note { margin: 8px 0 0; color: var(--el-text-color-secondary); }
.permission-raw { display: flex; flex-wrap: wrap; gap: 8px; margin-top: 10px; padding-top: 8px; border-top: 1px solid var(--el-border-color-lighter); color: var(--el-text-color-secondary); }
.permission-raw code { font-family: 'Cascadia Mono', Consolas, monospace; font-size: 12px; color: var(--el-text-color-primary); overflow-wrap: anywhere; }
</style>
