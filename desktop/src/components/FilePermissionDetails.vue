<script setup lang="ts">
// 权限详情：文件列表里鼠标停在「权限」格上时，在列表共用的悬停提示里显示（列表行里只显示 filePermissions 的 summary）。
import type { FilePermissionInfo } from '../filePermissions'

defineProps<{ permissions: FilePermissionInfo; mode: string; dir: boolean }>()
</script>

<template>
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

<style scoped>
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
