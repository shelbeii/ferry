<script setup lang="ts">
import { computed, defineAsyncComponent, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { call, FerryError, type Software, type SoftwareConfigs, type SoftwareConfigFile, type FilePreview } from '../ferry'
import { secretRequest, trustRequest } from '../state'
import FilePreviewDialog from './FilePreviewDialog.vue'
const ConfigEditor = defineAsyncComponent(() => import('./ConfigEditor.vue'))
const props = defineProps<{ config: string; target: string; software: Software }>()
const emit = defineEmits<{close: []; changed: []}>()
const visible = ref(true)
const loading = ref(false)
const working = ref('')
const error = ref('')
const report = ref<SoftwareConfigs>()
const selected = ref<SoftwareConfigFile>()
const projectConfig = ref(false)
const projectConfigUsed = ref(false)
watch(projectConfig, (open) => { if (open) projectConfigUsed.value = true })
const identity = computed(() => props.software.instance ? [props.software.instance] : props.software.container ? ['--container', props.software.container] : props.software.unit ? ['--unit', props.software.unit] : ['--pid', String(props.software.pid)])
const args = (op: string, file?: SoftwareConfigFile) => ['software', 'config', op, props.target, ...identity.value, ...(file ? ['--id', file.id] : [])]
function handle(e: unknown) {
  const err = e as FerryError
  if (err instanceof FerryError && err.needSecret && err.detail) secretRequest.value = { detail: err.detail, config: props.config, retry: load }
  if (err instanceof FerryError && err.unknownHost) trustRequest.value = { target: props.target, config: props.config }
  return err.message
}
async function load() {
  loading.value = true; error.value = ''
  try { report.value = await call<SoftwareConfigs>([...args('list'), '-c', props.config]) }
  catch (e) { error.value = handle(e) }
  finally { loading.value = false }
}
async function associate() {
  let path: string
  try { ({ value: path } = await ElMessageBox.prompt('配置文件应位于此软件安装目录、对应的 /etc 配置目录，或容器的持久化挂载内。', '关联配置文件', { inputPlaceholder: '服务器上的绝对路径', inputValidator: (value) => value.startsWith('/') || '请输入绝对路径' })) } catch { return }
  try { report.value = await call<SoftwareConfigs>([...args('associate'), '-c', props.config], { FERRY_FILE_VALUE: JSON.stringify({ path: path.trim() }) }); ElMessage.success('配置文件已关联') } catch (e) { ElMessage.error(handle(e)) }
}
async function apply(file: SoftwareConfigFile) {
  try { await ElMessageBox.confirm(file.apply === 'reload' ? '校验配置并重新加载此软件？' : `重启 ${props.software.name} 使配置生效？连接可能短暂中断。`, '配置生效', { confirmButtonText: file.apply === 'reload' ? '校验并重新加载' : '重启', type: 'warning' }) } catch { return }
  working.value = file.id
  try { await call([...args('apply', file), '-c', props.config]); ElMessage.success('配置已生效'); emit('changed') }
  catch (e) { ElMessage.error({ message: handle(e), duration: 8000 }) }
  finally { working.value = '' }
}
async function restore(file: SoftwareConfigFile) {
  const doc = await call<FilePreview>([...args('read', file), '-c', props.config]).catch((e) => { ElMessage.error(handle(e)); return undefined })
  if (!doc) return
  try { await ElMessageBox.confirm('恢复到上一次保存前的内容？当前内容会另存为备份，恢复后仍需执行生效操作。', '恢复配置备份', { confirmButtonText: '恢复', type: 'warning' }) } catch { return }
  working.value = file.id
  try { await call([...args('restore', file), '-c', props.config], { FERRY_FILE_VALUE: JSON.stringify({ revision: doc.revision }) }); ElMessage.success('已恢复配置文件'); await load() }
  catch (e) { ElMessage.error(handle(e)) }
  finally { working.value = '' }
}
watch(() => [props.config, props.target, props.software], () => { selected.value = undefined; load() }, { immediate: true })
</script>

<template>
  <el-dialog v-model="visible" :title="`${software.name} · 配置`" width="min(1000px, 94vw)" top="5vh" append-to-body :close-on-click-modal="false" @closed="emit('close')">
    <div class="config-heading"><span class="mono muted">{{ software.instance || software.unit || software.container || software.path }}</span><div><el-button :disabled="loading || !!working" @click="associate">关联配置文件</el-button><el-button :loading="loading" @click="load">刷新配置列表</el-button></div></div>
    <el-alert v-if="error" :title="error" type="error" :closable="false" />
    <el-alert v-for="note in report?.notes ?? []" :key="note" :title="note" type="info" :closable="false" class="config-note" />
    <div v-loading="loading" class="config-list">
      <article v-for="file in report?.files ?? []" :key="file.id" class="config-file">
        <div class="config-file-title"><strong>{{ file.label }}</strong><el-tag v-if="file.readOnly" size="small" type="info">由项目管理</el-tag><el-tag v-if="file.inPlace" size="small" type="info">容器文件挂载</el-tag></div>
        <p class="mono config-path">{{ file.path }}</p>
        <p v-if="file.readOnly" class="muted">{{ file.readOnly }}</p>
        <div class="config-actions"><el-button size="small" type="primary" plain :disabled="!!working" @click="selected = file">{{ file.readOnly ? '查看内容' : '查看 / 编辑' }}</el-button><el-button v-if="!file.readOnly && file.apply" size="small" :loading="working === file.id" :disabled="!!working" @click="apply(file)">{{ file.apply === 'reload' ? '校验并重新加载' : '重启生效' }}</el-button><el-button v-if="file.backup && !file.readOnly" size="small" :disabled="!!working" @click="restore(file)">恢复上次备份</el-button></div>
      </article>
      <el-empty v-if="!loading && !report?.files.length && !error" description="未找到可确认的配置文件" />
    </div>
    <template #footer><el-button v-if="software.manager === 'Ferry' && !software.instance && !config.endsWith('connections.yaml')" @click="projectConfig = true">打开项目配置</el-button><el-button @click="visible = false">关闭</el-button></template>
  </el-dialog>
  <FilePreviewDialog v-if="selected" :key="selected.id" :config="config" :target="target" :path="selected.path" :read-args="args('read', selected)" :save-args="args('save', selected)" :downloadable="false" @close="selected = undefined" @saved="load" />
  <ConfigEditor v-if="projectConfigUsed" v-model="projectConfig" :path="config" />
</template>

<style scoped>
.config-heading, .config-file-title, .config-actions { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
.config-heading { justify-content: space-between; margin-bottom: 12px; }
.config-note { margin: 8px 0; }
.config-list { min-height: 120px; max-height: 60vh; overflow-y: auto; }
.config-file { border: 1px solid var(--el-border-color-light); border-radius: 8px; padding: 14px; margin: 10px 0; }
.config-path { overflow-wrap: anywhere; margin: 8px 0; }
</style>
