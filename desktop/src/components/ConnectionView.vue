<script setup lang="ts">
// 「新建连接」保存的服务器：在右边显示连接信息和终端（可以开多个），文件点「文件」、连着的 SSH 点「登录用户」后从右边滑出。
import { computed, defineAsyncComponent, onActivated, onBeforeUnmount, onDeactivated, onMounted, reactive, ref, watch } from 'vue'
import { ElMessageBox } from 'element-plus'
import { ChatDotRound, Close, Delete, EditPen, FolderOpened, Monitor, Plus, SwitchButton, User } from '@element-plus/icons-vue'
import { type Connection } from '../ferry'
import { connGen, freeNumber, hangupConnection, newTermTab, pageTerminals, type TermTab } from '../state'
import SshPanel from './SshPanel.vue'
import ServerInfoPanel from './ServerInfoPanel.vue'

// 用到时才加载（编辑器、终端、AI 助手和各个对话框不放进启动时加载的代码里）
const TerminalView = defineAsyncComponent(() => import('./TerminalView.vue'))
const FilesDrawer = defineAsyncComponent(() => import('./FilesDrawer.vue'))
const AiDrawer = defineAsyncComponent(() => import('./AiDrawer.vue'))
const SoftwarePanel = defineAsyncComponent(() => import('./SoftwarePanel.vue'))

const props = defineProps<{ conn: Connection; config: string }>()
const emit = defineEmits<{ edit: []; remove: [] }>()

const tabs = reactive<TermTab[]>([])
const active = ref(0)
const shown = ref(true)
const filesOpen = ref(false)
const softwareOpen = ref(false)
const sshOpen = ref(false)
const sshUsed = ref(false) // 第一次打开时才去查
const sshPanel = ref<InstanceType<typeof SshPanel>>()
const serverOpen = ref(false)
const serverUsed = ref(false)
const serverPanel = ref<InstanceType<typeof ServerInfoPanel>>()
const aiOpen = ref(false)
const aiUsed = ref(false) // 第一次打开后才创建（之后关掉再打开还是原来的对话）

const addr = computed(() => `${props.conn.user}@${props.conn.host}:${props.conn.port}`)
// 有终端连着（或正在连接）：显示「断开」
const live = computed(() => tabs.some((t) => t.state !== 'closed'))

function newTab() {
  // 用最小的空号：关掉终端 1 后再开，还叫终端 1
  const num = freeNumber(tabs)
  const t = newTermTab(props.config, props.conn.name, `终端 ${num}`, num)
  tabs.push(t)
  pageTerminals.push(t)
  active.value = t.id
}

async function closeTab(t: TermTab) {
  if (t.state === 'open') {
    try {
      await ElMessageBox.confirm(`关闭「${t.title}」？里面正在运行的命令会被结束。`, '关闭终端', { type: 'warning', confirmButtonText: '关闭', lockScroll: false })
    } catch {
      return
    }
  }
  drop(t)
  const i = tabs.indexOf(t)
  tabs.splice(i, 1)
  if (active.value === t.id) active.value = tabs[Math.min(i, tabs.length - 1)]?.id ?? 0
}

function drop(t: TermTab) {
  const j = pageTerminals.indexOf(t)
  if (j >= 0) pageTerminals.splice(j, 1)
}

// 第一次打开这个连接时自动连上一个终端
onMounted(newTab)

// 这个连接被删了：关掉所有终端（断开连接）。页面还在后台保留着，但不会再显示（再建同名的连接是新的页面）
const gen = connGen[props.conn.name] ?? 0
watch(
  () => connGen[props.conn.name] ?? 0,
  (g) => {
    if (g === gen) return
    tabs.forEach(drop)
    tabs.splice(0)
    filesOpen.value = sshOpen.value = serverOpen.value = aiOpen.value = false
  },
)
onActivated(() => (shown.value = true))
onDeactivated(() => (shown.value = false))
onBeforeUnmount(() => tabs.forEach(drop))
</script>

<template>
  <div class="conn-page">
    <div class="head">
      <div>
        <h2>{{ conn.title }}</h2>
        <div class="muted mono">{{ addr }}</div>
      </div>
      <div class="head-actions">
        <el-button @click="softwareOpen = true">软件</el-button>
        <el-tag size="small" :type="conn.auth === 'password' ? 'warning' : 'success'">{{ conn.auth === 'password' ? '密码登录' : '密钥登录' }}</el-tag>
        <el-tooltip :show-after="HOVER_DELAY" content="上传、下载、新建文件夹、改名、删除（以登录用户的身份）" placement="bottom">
          <el-button type="primary" :icon="FolderOpened" @click="filesOpen = true">文件</el-button>
        </el-tooltip>
        <el-tooltip :show-after="HOVER_DELAY" content="谁连着这台服务器：登录用户、来源 IP、连了多久、在干什么，可以断开" placement="bottom">
          <el-button :icon="User" @click="(sshUsed = true), (sshOpen = true)">登录用户</el-button>
        </el-tooltip>
        <el-tooltip :show-after="HOVER_DELAY" content="系统、CPU、内存和各挂载点的资源使用情况" placement="bottom">
          <el-button :icon="Monitor" @click="(serverUsed = true), (serverOpen = true)">服务器信息</el-button>
        </el-tooltip>
        <el-tooltip :show-after="HOVER_DELAY" content="让 AI 查看、排查这台服务器；允许改动时，每个改动都先问你" placement="bottom">
          <el-button :icon="ChatDotRound" @click="(aiUsed = true), (aiOpen = true)">AI 助手</el-button>
        </el-tooltip>
        <el-tooltip :show-after="HOVER_DELAY" content="断开这台服务器的所有终端和 SSH 连接（终端标签留着，按回车重新连接）" placement="bottom">
          <el-button v-if="live" :icon="SwitchButton" @click="hangupConnection(conn.name, conn.title)">断开</el-button>
        </el-tooltip>
        <el-button :icon="EditPen" @click="emit('edit')">修改</el-button>
        <el-button :icon="Delete" text type="danger" @click="emit('remove')">删除</el-button>
      </div>
    </div>

    <div class="term-card">
      <div class="term-tabs">
        <div v-for="t in tabs" :key="t.id" class="tab" :class="{ active: t.id === active }" @click="active = t.id" @mousedown.middle.prevent="closeTab(t)">
          <span class="led" :class="t.state" />
          <span>{{ t.title }}</span>
          <el-icon class="x" @click.stop="closeTab(t)"><Close /></el-icon>
        </div>
        <el-tooltip :show-after="HOVER_DELAY" content="再开一个终端" placement="bottom">
          <el-button text size="small" :icon="Plus" class="add" @click="newTab" />
        </el-tooltip>
      </div>
      <div class="term-body">
        <TerminalView v-for="t in tabs" v-show="t.id === active" :key="t.id" :tab="t" :active="shown && t.id === active" no-files />
        <div v-if="!tabs.length" class="empty">
          <el-button type="primary" :icon="Plus" @click="newTab">打开终端</el-button>
        </div>
      </div>
    </div>

    <FilesDrawer v-model="filesOpen" :config="config" :target="conn.name" :title="conn.title" />
    <el-drawer v-if="softwareOpen" v-model="softwareOpen" direction="rtl" size="94vw" append-to-body :title="`软件：${conn.title}`"><SoftwarePanel :config="config" :target="conn.name" :host="conn.host" :connected="live" /></el-drawer>
    <el-drawer v-if="sshUsed" v-model="sshOpen" direction="rtl" size="64%" append-to-body :title="`登录用户：${conn.title}`" class="ssh-drawer" @open="sshPanel?.load()">
      <SshPanel ref="sshPanel" :config="config" :target="conn.name" :connected="tabs.some((t) => t.state === 'open')" bar />
    </el-drawer>
    <el-drawer v-if="serverUsed" v-model="serverOpen" direction="rtl" size="80%" append-to-body :title="`服务器信息：${conn.title}`" @open="serverPanel?.load()">
      <ServerInfoPanel ref="serverPanel" :config="config" :target="conn.name" :connected="tabs.some((t) => t.state === 'open')" :active="shown && serverOpen" bar />
    </el-drawer>
    <AiDrawer v-if="aiUsed" v-model="aiOpen" :config="config" :target="conn.name" :title="conn.title" plain />
  </div>
</template>

<style scoped>
.conn-page {
  display: flex;
  flex-direction: column;
  gap: 12px;
  height: 100%;
  min-height: 0;
}
.head {
  display: flex;
  justify-content: space-between;
  align-items: flex-start;
}
h2 {
  margin: 0 0 4px;
}
.head-actions {
  display: flex;
  flex-wrap: wrap;
  justify-content: flex-end;
  align-items: center;
  gap: 8px;
}
.head-actions .el-button + .el-button {
  margin-left: 0;
}
.term-card {
  flex: 1;
  min-height: 300px;
  display: flex;
  flex-direction: column;
  border-radius: var(--ferry-radius);
  overflow: hidden;
  border: 1px solid #333;
  background: #1e1e1e;
}
.term-tabs {
  display: flex;
  align-items: center;
  gap: 2px;
  height: 32px;
  padding: 0 6px;
  background: #252526;
}
.tab {
  display: flex;
  align-items: center;
  gap: 6px;
  height: 26px;
  padding: 0 8px 0 10px;
  border-radius: 4px 4px 0 0;
  font-size: 13px;
  cursor: pointer;
  color: #aaa;
}
.tab:hover {
  background: #2d2d2d;
}
.tab.active {
  background: #1e1e1e;
  color: #fff;
}
.x {
  font-size: 12px;
  opacity: 0.6;
}
.x:hover {
  opacity: 1;
}
.led {
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: #777;
}
.led.open {
  background: #4caf50;
}
.led.connecting {
  background: #e6a23c;
}
.add {
  color: #ccc;
}
.term-body {
  flex: 1;
  min-height: 0;
  position: relative;
}
.empty {
  height: 100%;
  display: flex;
  align-items: center;
  justify-content: center;
}
</style>
