<script setup lang="ts">
// 执行改动前确认：列出要执行的语句，危险的（删表、不带 WHERE 的删改）标出来。
import { computed } from 'vue'
import { ElMessage } from 'element-plus'
import { CopyDocument, WarningFilled } from '@element-plus/icons-vue'
import { dangerOf, sqlConfirm } from '../../db'

const req = computed(() => sqlConfirm.value)
const dangers = computed(() => [...new Set((req.value?.stmts ?? []).map(dangerOf).filter(Boolean))])
const text = computed(() => (req.value?.stmts ?? []).map((s) => s.trim().replace(/;$/, '') + ';').join('\n\n'))

function done(ok: boolean) {
  const r = sqlConfirm.value
  sqlConfirm.value = null
  r?.resolve(ok)
}
async function copy() {
  await navigator.clipboard.writeText(text.value)
  ElMessage.success({ message: '已复制', duration: 1200 })
}
</script>

<template>
  <el-dialog :model-value="!!req" :title="req?.title" width="720px" append-to-body :close-on-click-modal="false" @close="done(false)">
    <template v-if="req">
      <div class="where">
        在 <b>{{ req.conn.name }}</b> 上执行 {{ req.stmts.length }} 条语句{{ req.conn.via ? `（经服务器 ${req.conn.via.title || req.conn.via.target}）` : '' }}：
      </div>
      <el-alert v-for="d in dangers" :key="d" type="error" :closable="false" show-icon class="danger">
        <template #title><el-icon><WarningFilled /></el-icon> 注意：{{ d }}，做了就改不回来</template>
      </el-alert>
      <pre class="sql">{{ text }}</pre>
      <div v-if="req.note" class="note">{{ req.note }}</div>
    </template>
    <template #footer>
      <el-button :icon="CopyDocument" @click="copy">复制语句</el-button>
      <el-button @click="done(false)">取消</el-button>
      <el-button :type="dangers.length ? 'danger' : 'primary'" @click="done(true)">{{ req?.okText || '执行' }}</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.where {
  margin-bottom: 8px;
  color: var(--el-text-color-regular);
}
.danger {
  margin-bottom: 8px;
}
.sql {
  max-height: 46vh;
  overflow: auto;
  margin: 0;
  padding: 10px 12px;
  background: var(--el-fill-color-lighter);
  border: 1px solid var(--el-border-color-lighter);
  border-radius: 6px;
  font-family: 'Cascadia Mono', Consolas, monospace;
  font-size: 12.5px;
  line-height: 1.55;
  white-space: pre-wrap;
  word-break: break-all;
}
.note {
  margin-top: 8px;
  color: var(--el-text-color-secondary);
  font-size: 12.5px;
}
</style>
