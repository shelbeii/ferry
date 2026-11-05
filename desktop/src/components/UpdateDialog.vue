<script setup lang="ts">
import { computed } from 'vue'
import { ElMessage } from 'element-plus'
import MarkdownIt from 'markdown-it'
import { bytes } from '../ferry'
import { exitApp, openUrl } from '../platform'
import { pageTerminals, tasks, terminals } from '../state'
import { cancelUpdate, installUpdate, updater } from '../update'

// 新版本：更新了什么、点「更新」下载安装包并安装
const info = computed(() => updater.info)
const current = computed(() => info.value?.current.replace(/\+.*$/, '') ?? '')
const running = computed(() => tasks.filter((t) => t.status === 'running').length)
const openTerms = computed(() => [...terminals, ...pageTerminals].filter((t) => t.state === 'open').length)
const busyText = computed(() => {
  const parts = [running.value ? `${running.value} 个任务在进行中` : '', openTerms.value ? `${openTerms.value} 个终端连着` : ''].filter(Boolean)
  return parts.length ? `还有${parts.join('、')}：更新时 Ferry 会关闭，本机这一侧的任务会停止（服务器上已经开始的切换会继续完成），终端里正在运行的命令会被结束。` : ''
})
const percent = computed(() => (updater.total > 0 ? Math.min(100, Math.floor((updater.done / updater.total) * 100)) : 0))
const published = computed(() => {
  const t = info.value?.published ? new Date(info.value.published) : undefined
  return t && !isNaN(t.getTime()) ? t.toLocaleDateString() : ''
})
const action = computed(() => (info.value?.install === 'dmg' ? '下载并打开安装包' : '更新'))

// 发布说明按 Markdown 显示：不放行 HTML，链接只认 http(s)，用系统浏览器打开
const markdown = new MarkdownIt({ html: false, linkify: true, breaks: true })
markdown.validateLink = (url) => /^https?:\/\//i.test(url.trim())
const notes = computed(() => (info.value?.notes ? markdown.render(info.value.notes) : ''))
function onNotesClick(e: MouseEvent) {
  const a = (e.target as HTMLElement).closest('a')
  if (!a) return
  e.preventDefault()
  const href = a.getAttribute('href') ?? ''
  if (/^https?:\/\//i.test(href)) openUrl(href).catch((err) => ElMessage.error(String(err)))
}

function close() {
  if (updater.phase === 'download' || updater.phase === 'install') return
  updater.dialog = false
}
</script>

<template>
  <el-dialog
    :model-value="updater.dialog"
    :title="info?.newer ? 'Ferry 有新版本' : '检查更新'"
    width="620px"
    append-to-body
    :close-on-click-modal="false"
    :show-close="updater.phase !== 'download' && updater.phase !== 'install'"
    @update:model-value="(v: boolean) => !v && close()"
  >
    <template v-if="info">
      <div class="head">
        <div>
          <div class="ver">{{ info.latest }}</div>
          <div class="muted small">这台电脑上是 {{ current }}<span v-if="published">　·　{{ published }} 发布</span><span v-if="info.size">　·　安装包 {{ bytes(info.size) }}</span></div>
        </div>
        <el-button link type="primary" @click="openUrl(info.page)">在 GitHub 上查看</el-button>
      </div>

      <div v-if="notes" class="notes md" @click="onNotesClick" v-html="notes" />
      <p v-else class="muted">没取到更新说明，可以到 GitHub 的发布页查看。</p>

      <el-alert v-if="!info.install" type="info" :closable="false" show-icon class="mt" title="这种系统没有现成的安装包，请到发布页下载。" />
      <el-alert v-else-if="busyText && !updater.phase" type="warning" :closable="false" show-icon class="mt" :title="busyText" />

      <div v-if="updater.phase === 'download'" class="mt">
        <el-progress :percentage="percent" :indeterminate="!updater.total" :duration="2" />
        <div class="muted small">
          正在下载{{ updater.total ? `　${bytes(updater.done)} / ${bytes(updater.total)}` : `　${bytes(updater.done)}` }}
        </div>
      </div>
      <el-alert v-else-if="updater.phase === 'install'" type="success" :closable="false" show-icon class="mt" title="正在启动安装程序：Ferry 马上关闭，装好后自动重新打开" />
      <el-alert v-else-if="updater.phase === 'opened'" type="success" :closable="false" show-icon class="mt" title="安装包已打开">
        <p>退出 Ferry，把安装包里的 Ferry 拖到「应用程序」里替换旧的，再打开 Ferry。</p>
      </el-alert>
      <el-alert v-if="updater.error" type="error" :closable="false" show-icon class="mt" title="更新失败">
        <p class="pre">{{ updater.error }}</p>
      </el-alert>
    </template>

    <template #footer>
      <template v-if="updater.phase === 'download'">
        <el-button @click="cancelUpdate">取消下载</el-button>
      </template>
      <template v-else-if="updater.phase === 'opened'">
        <el-button @click="close">稍后</el-button>
        <el-button type="primary" @click="exitApp">退出 Ferry</el-button>
      </template>
      <template v-else-if="updater.phase !== 'install'">
        <el-button @click="close">以后再说</el-button>
        <el-button v-if="info?.newer && !info.install" type="primary" @click="openUrl(info.page)">去发布页下载</el-button>
        <el-button v-else-if="info?.newer" type="primary" @click="installUpdate">{{ updater.error ? '重试' : action }}</el-button>
      </template>
    </template>
  </el-dialog>
</template>

<style scoped>
.head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 12px;
  margin-bottom: 12px;
}
.ver {
  font-size: 22px;
  font-weight: 600;
  line-height: 1.3;
}
.notes {
  max-height: 320px;
  overflow: auto;
  padding: 10px 14px;
  border: 1px solid var(--el-border-color-lighter);
  border-radius: 8px;
  background: var(--el-fill-color-lighter);
  line-height: 1.6;
  overflow-wrap: anywhere;
}
.notes :deep(h1),
.notes :deep(h2),
.notes :deep(h3) {
  font-size: 14px;
  margin: 10px 0 6px;
}
.notes :deep(ul),
.notes :deep(ol) {
  padding-left: 20px;
  margin: 4px 0;
}
.notes :deep(p) {
  margin: 4px 0;
}
.notes :deep(code) {
  font-family: var(--ferry-mono, monospace);
  font-size: 12px;
}
.mt {
  margin-top: 12px;
}
.small {
  font-size: 12px;
}
.pre {
  white-space: pre-wrap;
  margin: 0;
}
</style>
