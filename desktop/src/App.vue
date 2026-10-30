<script setup lang="ts">
import { computed, defineAsyncComponent, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { pickConfig, pickSaveFile } from './platform'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Plus, Delete, List, Setting, Monitor, Fold, Expand, FolderOpened, MoreFilled, EditPen, SwitchButton, Search } from '@element-plus/icons-vue'
import { ask } from '@tauri-apps/plugin-dialog'
import { getCurrentWindow } from '@tauri-apps/api/window'
import { isTauri } from './platform'
import { call, FerryError, type Connection, type Inspect } from './ferry'
import { connectionsPath as connPath, CONN_PREFIX, pageTerminals, loadTaskHistory } from './state'
import { projects, currentPath, loadProjects, addProject, removeProject, relocateProject, selectProject, connGen, hangupConnection, tasks, drawerOpen, localRequest, trustRequest, trustedTick, secretRequest, busyRequest, settingsRequest, appSettings, terminals, terminalShown, terminalHeight, connections, loadConnections, projectSummary, uiPref, setUiPref } from './state'
import ProjectView from './components/ProjectView.vue'
import TaskDrawer from './components/TaskDrawer.vue'
import TrustDialog from './components/TrustDialog.vue'
import SecretDialog from './components/SecretDialog.vue'
import TerminalHost from './components/TerminalHost.vue'
import ConnectionView from './components/ConnectionView.vue'
import McpApprovals from './components/McpApprovals.vue'

// 用到时才加载（编辑器、终端、AI 助手和各个对话框不放进启动时加载的代码里）
const SettingsDialog = defineAsyncComponent(() => import('./components/SettingsDialog.vue'))
const NewProjectDialog = defineAsyncComponent(() => import('./components/NewProjectDialog.vue'))
const ConnectionDialog = defineAsyncComponent(() => import('./components/ConnectionDialog.vue'))
const BusyDialog = defineAsyncComponent(() => import('./components/BusyDialog.vue'))

const settingsOpen = ref(false)
watch(settingsRequest, (t) => {
  if (t) settingsOpen.value = true
})
const newOpen = ref(false)
/** 新建连接（null）/ 修改某个连接；undefined = 不显示 */
const connEdit = ref<Connection | null | undefined>(undefined)

/** 项目列表和保存的连接读完了：之前不显示「还没有项目」，免得启动时闪一下 */
const loaded = ref(false)
const startupError = ref('')
const navigationQuery = ref('')
const navigationSearch = ref<{ focus: () => void }>()
function focusNavigation(event: KeyboardEvent) {
  if (event.defaultPrevented || (event.target instanceof Element && event.target.closest('.el-dialog, .el-drawer'))) return
  if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === 'k') {
    event.preventDefault()
    listOpen.value = true
    nextTick(() => navigationSearch.value?.focus())
  }
}
onMounted(() => window.addEventListener('keydown', focusNavigation))
onBeforeUnmount(() => window.removeEventListener('keydown', focusNavigation))
const matchingProjects = computed(() => projects.value.filter((p) => `${p.name} ${p.path}`.toLowerCase().includes(navigationQuery.value.trim().toLowerCase())))
const matchingConnections = computed(() => connections.value.filter((c) => `${c.title} ${c.host} ${c.user}`.toLowerCase().includes(navigationQuery.value.trim().toLowerCase())))

async function initialize() {
  loaded.value = false
  startupError.value = ''
  try {
    await Promise.all([
      loadProjects(),
      loadConnections().catch((e) => ElMessage.warning({ message: `读取保存的连接失败：${(e as Error).message}`, duration: 6000 })),
    ])
  } catch (e) {
    startupError.value = (e as Error).message
  } finally {
    loaded.value = true
  }
  // 以前的任务（重启后还能看到）
  loadTaskHistory().catch((e) => ElMessage.warning({ message: `读取任务记录失败：${(e as Error).message}`, duration: 6000 }))
}
onMounted(async () => {
  await initialize()
  if (isTauri) {
    // 有任务进行中时关闭窗口先确认（设置 → 界面）
    await getCurrentWindow().onCloseRequested(async (e) => {
      const n = tasks.filter((t) => t.status === 'running').length
      const nt = [...terminals, ...pageTerminals].filter((t) => t.state === 'open').length
      if ((n === 0 && nt === 0) || appSettings.value?.ui.confirmClose === false) return
      const what = [n ? `还有 ${n} 个任务在进行中。关闭后本机这一侧会停止（服务器上已经开始的切换会继续完成）。` : '', nt ? `还有 ${nt} 个终端连着，关闭后里面正在运行的命令会被结束。` : '']
      const ok = await ask(`${what.join('')}确定关闭？`, { title: 'Ferry', kind: 'warning' })
      if (!ok) e.preventDefault()
    })
  }
})

const current = computed(() => projects.value.find((p) => p.path === currentPath.value))
/** 选中的是「新建连接」保存的服务器 */
const currentConn = computed(() =>
  currentPath.value.startsWith(CONN_PREFIX) ? connections.value.find((c) => CONN_PREFIX + c.name === currentPath.value) : undefined,
)
const running = computed(() => tasks.filter((t) => t.status === 'running').length)

// 项目列表可以收起，只留最左边的图标栏（记在本机）
const RAIL = 56
const LIST = 208
const listOpen = ref(uiPref('listOpen') !== '0')
watch(listOpen, (v) => setUiPref('listOpen', v ? '1' : '0'))
// 终端面板等按左边栏的宽度定位
const sideWidth = computed(() => RAIL + (listOpen.value ? LIST : 0))

// 项目列表：每个项目一个带颜色的首字母方块，颜色按名字固定，好区分
const AVATAR_COLORS = ['#4f46e5', '#0891b2', '#059669', '#d97706', '#db2777', '#7c3aed', '#2563eb', '#dc2626']
// 记录了主机公钥：页面重新读取；是任务碰到的，接着执行那个任务（关窗口会清掉 trustRequest，先取出来）
function onTrusted() {
  const retry = trustRequest.value?.retry
  trustedTick.value++
  retry?.()
}

function avatarStyle(name: string) {
  let h = 0
  for (const ch of name) h = (h * 31 + ch.charCodeAt(0)) >>> 0
  return { background: AVATAR_COLORS[h % AVATAR_COLORS.length] }
}
const initial = (name: string) => (name.trim()[0] ?? '?').toUpperCase()
// 第二行：还没读过状态时显示配置文件所在的文件夹
const folderOf = (path: string) => path.split(/[\\/]/).slice(-2, -1)[0] ?? ''

function onAdd(c: string) {
  if (c === 'new') newOpen.value = true
  else if (c === 'conn') connEdit.value = null
  else add()
}

async function add() {
  const path = await pickConfig()
  if (!path) return
  try {
    const info = await call<Inspect>(['inspect', '-c', path])
    await addProject({ path, name: info.project })
    if (info.problems?.length) localRequest.value = path // 这台电脑上缺私钥、源码目录：打开本机设置
  } catch (e) {
    // 配置有错误也允许添加，进入项目后可以在配置编辑器里修改
    await addProject({ path, name: path.split(/[\\/]/).slice(-2, -1)[0] ?? path })
    ElMessage.warning({ message: `配置有问题：${(e as Error).message}`, duration: 6000 })
  }
}

async function removeConn(c: Connection) {
  try {
    await ElMessageBox.confirm(`从列表里删除 ${c.title}（${c.user}@${c.host}）？记住的密码不会删除。`, '删除连接', { type: 'warning', confirmButtonText: '删除' })
  } catch {
    return
  }
  try {
    await call(['connection', 'remove', c.name])
    // 后台保留着的这个连接的页面关掉终端；再建同名的连接时是新的页面
    connGen[c.name] = (connGen[c.name] ?? 0) + 1
    await loadConnections()
    if (currentPath.value === CONN_PREFIX + c.name) await selectProject(projects.value[0]?.path ?? '')
  } catch (e) {
    ElMessage.error((e as Error).message)
  }
}

// 连接的状态：有终端连着时头像右下角显示绿点，正在连接时显示橙点
const connState = computed(() => {
  const out: Record<string, { open: number; connecting: number }> = {}
  for (const t of pageTerminals) {
    if (t.config !== connPath.value || t.state === 'closed') continue
    const s = (out[t.target] ??= { open: 0, connecting: 0 })
    if (t.state === 'open') s.open++
    else s.connecting++
  }
  return out
})

function connTip(c: Connection): string {
  const s = connState.value[c.name]
  const state = !s ? '没有连接' : s.open ? `已连接（${s.open} 个终端）` : '正在连接…'
  return `${c.user}@${c.host}:${c.port} · ${state}`
}

// 列表每一行的操作收在「⋯」菜单里（右键也能打开）：选项目时不会误点到删除
const menus = new Map<string, { handleOpen: () => void }>()
const menuOpen = ref('')
function setMenu(key: string, el: unknown) {
  if (el) menus.set(key, el as { handleOpen: () => void })
  else menus.delete(key)
}
function openMenu(key: string) {
  menus.get(key)?.handleOpen()
}

async function remove(path: string) {
  try {
    await ElMessageBox.confirm('只是从列表中移除，不会删除配置文件。', '移除项目', { type: 'warning' })
  } catch {
    return
  }
  await removeProject(path)
}

// 移动配置文件：本机设置、部署历史、构建日志是按它的位置记的，由 ferry move-config 一起搬过去
async function moveConfig(path: string) {
  if (tasks.some((t) => t.status === 'running' && t.config === path)) {
    ElMessage.warning('这个项目还有任务在进行，等它们做完再移动')
    return
  }
  const to = await pickSaveFile('把 ferry.yaml 移到哪里', path, 'Ferry 配置', ['yaml', 'yml'])
  if (!to || to === path) return
  let force = false
  for (;;) {
    try {
      const r = await call<{ to: string; dataDir?: string }>(['move-config', to, '-c', path, ...(force ? ['--force'] : [])])
      await relocateProject(path, r.to)
      ElMessage.success(r.dataDir ? `已移到 ${r.to}，本机设置、部署历史、构建日志都跟着搬过去了` : `已移到 ${r.to}`)
      return
    } catch (e) {
      const msg = (e as Error).message
      if (force || !(e instanceof FerryError) || !msg.includes('--force')) {
        ElMessageBox.alert(msg, '移动配置文件失败', { type: 'error', customStyle: { whiteSpace: 'pre-line' } })
        return
      }
      try {
        const text = msg.replace(/\n先把它们改成绝对路径再移动.*$/s, '\n建议先把它们改成绝对路径再移动；照样移动的话，移过去以后要自己改。')
        await ElMessageBox.confirm(text, '有按所在文件夹写的路径', {
          type: 'warning',
          confirmButtonText: '照样移动',
          cancelButtonText: '先不移动',
          customStyle: { whiteSpace: 'pre-line' },
        })
      } catch {
        return
      }
      force = true
    }
  }
}
</script>

<template>
  <el-container class="shell" :style="{ '--ferry-side': sideWidth + 'px' }">
    <!-- 最左边的图标栏：收起 / 展开项目列表、添加、任务、设置 -->
    <nav class="rail" aria-label="主导航">
      <img src="/logo.png" alt="Ferry" class="logo" />
      <el-tooltip :show-after="HOVER_DELAY" :content="listOpen ? '收起项目列表' : '展开项目列表'" placement="right">
        <button class="rail-btn" :class="{ on: listOpen }" :aria-label="listOpen ? '收起项目列表' : '展开项目列表'" @click="listOpen = !listOpen">
          <el-icon :size="18"><Fold v-if="listOpen" /><Expand v-else /></el-icon>
        </button>
      </el-tooltip>
      <el-dropdown trigger="click" placement="right-start" @command="onAdd">
        <button class="rail-btn" aria-label="添加" title="新建项目、添加已有的 ferry.yaml 或新建连接">
          <el-icon :size="18"><Plus /></el-icon>
        </button>
        <template #dropdown>
          <el-dropdown-menu>
            <el-dropdown-item command="new">新建项目…</el-dropdown-item>
            <el-dropdown-item command="add">添加已有的 ferry.yaml</el-dropdown-item>
            <el-dropdown-item command="conn" divided>新建连接（终端、文件）…</el-dropdown-item>
          </el-dropdown-menu>
        </template>
      </el-dropdown>
      <span class="rail-gap" />
      <el-tooltip :show-after="HOVER_DELAY" :content="running ? `任务：${running} 个进行中` : '任务'" placement="right">
        <button class="rail-btn" aria-label="任务" @click="drawerOpen = true">
          <el-badge :value="running" :hidden="running === 0" type="primary" class="rail-badge">
            <el-icon :size="18"><List /></el-icon>
          </el-badge>
        </button>
      </el-tooltip>
      <el-tooltip :show-after="HOVER_DELAY" content="设置" placement="right">
        <button class="rail-btn" aria-label="设置" @click="settingsOpen = true">
          <el-icon :size="18"><Setting /></el-icon>
        </button>
      </el-tooltip>
    </nav>
    <aside v-show="listOpen" class="aside">
      <div class="workspace-brand"><strong>Ferry<span> / </span></strong><span>部署工作台</span></div>
      <div class="navigation-search"><el-input ref="navigationSearch" v-model="navigationQuery" :prefix-icon="Search" placeholder="搜索项目或连接" clearable size="small" aria-label="搜索项目或连接" title="Ctrl / ⌘ + K 快速定位" /></div>
      <div class="aside-title"><span>项目</span></div>
      <el-empty v-if="loaded && !startupError && projects.length === 0 && connections.length === 0" description="点最左边的 + 新建项目、添加已有的 ferry.yaml，或新建连接" :image-size="60" />
      <ul class="projects">
        <li
          v-for="p in matchingProjects"
          :key="p.path"
          :class="{ active: p.path === currentPath }"
          @click="selectProject(p.path)"
          role="button"
          tabindex="0"
          @keydown.enter="selectProject(p.path)"
          @keydown.space.prevent="selectProject(p.path)"
          @contextmenu.prevent="openMenu(p.path)"
        >
          <span class="pavatar" :style="avatarStyle(p.name)" aria-hidden="true">{{ initial(p.name) }}</span>
          <el-tooltip :show-after="HOVER_DELAY" :content="p.path" placement="right">
            <span class="ptext">
              <span class="pname">{{ p.name }}</span>
              <span class="psub">{{ projectSummary[p.path] ? `${projectSummary[p.path].up}/${projectSummary[p.path].total} 运行中` : folderOf(p.path) }}</span>
            </span>
          </el-tooltip>
          <el-dropdown
            :ref="(el: unknown) => setMenu(p.path, el)"
            trigger="click"
            placement="bottom-end"
            class="pmore"
            :class="{ open: menuOpen === p.path }"
            @visible-change="(v: boolean) => (menuOpen = v ? p.path : menuOpen === p.path ? '' : menuOpen)"
            @command="(c: string) => (c === 'move' ? moveConfig(p.path) : remove(p.path))"
          >
            <el-button :icon="MoreFilled" text size="small" aria-label="更多操作" @click.stop />
            <template #dropdown>
              <el-dropdown-menu>
                <el-dropdown-item command="move" :icon="FolderOpened">移动配置文件…</el-dropdown-item>
                <el-dropdown-item command="remove" :icon="Delete" divided class="danger-item">从列表移除</el-dropdown-item>
              </el-dropdown-menu>
            </template>
          </el-dropdown>
        </li>
      </ul>
      <template v-if="connections.length">
        <div class="aside-title sub"><span>连接</span></div>
        <ul class="projects">
          <li
            v-for="c in matchingConnections"
            :key="CONN_PREFIX + c.name"
            :class="{ active: CONN_PREFIX + c.name === currentPath }"
            @click="selectProject(CONN_PREFIX + c.name)"
            role="button"
            tabindex="0"
            @keydown.enter="selectProject(CONN_PREFIX + c.name)"
            @keydown.space.prevent="selectProject(CONN_PREFIX + c.name)"
            @contextmenu.prevent="openMenu(CONN_PREFIX + c.name)"
          >
            <span class="pavatar conn" aria-hidden="true">
              <el-icon><Monitor /></el-icon>
              <span v-if="connState[c.name]" class="cdot" :class="connState[c.name].open ? 'on' : 'connecting'" />
            </span>
            <el-tooltip :show-after="HOVER_DELAY" :content="connTip(c)" placement="right">
              <span class="ptext">
                <span class="pname">{{ c.title }}</span>
                <span class="psub mono">{{ c.user }}@{{ c.host }}</span>
              </span>
            </el-tooltip>
            <el-dropdown
              :ref="(el: unknown) => setMenu(CONN_PREFIX + c.name, el)"
              trigger="click"
              placement="bottom-end"
              class="pmore"
              :class="{ open: menuOpen === CONN_PREFIX + c.name }"
              @visible-change="(v: boolean) => (menuOpen = v ? CONN_PREFIX + c.name : menuOpen === CONN_PREFIX + c.name ? '' : menuOpen)"
              @command="(cmd: string) => (cmd === 'edit' ? (connEdit = c) : cmd === 'hangup' ? hangupConnection(c.name, c.title) : removeConn(c))"
            >
              <el-button :icon="MoreFilled" text size="small" aria-label="更多操作" @click.stop />
              <template #dropdown>
                <el-dropdown-menu>
                  <el-dropdown-item v-if="connState[c.name]" command="hangup" :icon="SwitchButton">断开连接</el-dropdown-item>
                  <el-dropdown-item command="edit" :icon="EditPen">修改连接…</el-dropdown-item>
                  <el-dropdown-item command="remove" :icon="Delete" divided class="danger-item">删除连接</el-dropdown-item>
                </el-dropdown-menu>
              </template>
            </el-dropdown>
          </li>
        </ul>
      </template>
      <p v-if="navigationQuery && !matchingProjects.length && !matchingConnections.length" class="nav-empty">没有找到匹配的项目或连接</p>
    </aside>
    <el-main class="main" :style="terminalShown && terminals.length ? { paddingBottom: terminalHeight + 16 + 'px' } : {}">
      <!-- 切换项目时保留各项目页（状态、所选目标），切回来不用从头加载 -->
      <!-- 连接页也保留：切到别的项目再切回来，终端还连着 -->
      <KeepAlive :max="20">
        <ProjectView v-if="current && !startupError" :key="current.path" :project="current" />
        <ConnectionView
          v-else-if="currentConn && !startupError"
          :key="`${CONN_PREFIX}${currentConn.name}#${connGen[currentConn.name] ?? 0}`"
          :conn="currentConn"
          :config="connPath"
          @edit="connEdit = currentConn"
          @remove="removeConn(currentConn)"
        />
      </KeepAlive>
      <el-alert v-if="startupError" type="error" :closable="false" show-icon title="工作台加载失败" class="startup-error"><p>{{ startupError }}</p><el-button type="primary" size="small" @click="initialize">重新加载工作台</el-button></el-alert>
      <div v-else-if="loaded && !current && !currentConn" class="welcome">
        <span class="welcome-kicker">FERRY WORKSPACE</span>
        <h1>从代码，到稳定运行。</h1>
        <p>连接服务器，部署应用，让每一次上线都有迹可循。</p>
        <div class="welcome-actions"><el-button type="primary" :icon="Plus" @click="newOpen = true">新建项目</el-button><el-button :icon="FolderOpened" @click="add">添加已有项目</el-button><el-button @click="connEdit = null">连接服务器</el-button></div>
        <div class="welcome-features"><span><b>01</b> 自动识别应用</span><span><b>02</b> 部署前检查</span><span><b>03</b> 失败自动回滚</span></div>
      </div>
    </el-main>
    <TaskDrawer />
    <TerminalHost />
    <McpApprovals />
    <SettingsDialog v-if="settingsOpen" @close="settingsOpen = false" />
    <NewProjectDialog v-if="newOpen" @close="newOpen = false" />
    <TrustDialog
      v-if="trustRequest && (trustRequest.config || current)"
      :config="trustRequest.config || current!.path"
      :target="trustRequest.target"
      :retrying="!!trustRequest.retry"
      @close="trustRequest = null"
      @trusted="onTrusted"
    />
    <ConnectionDialog v-if="connEdit !== undefined" :conn="connEdit ?? undefined" @close="connEdit = undefined" />
    <BusyDialog
      v-if="busyRequest"
      :config="busyRequest.config"
      :target="busyRequest.target"
      :items="busyRequest.items"
      @retry="busyRequest.retry()"
      @close="busyRequest = null"
    />
    <SecretDialog
      v-if="secretRequest"
      :key="secretRequest.detail.env + secretRequest.detail.kind"
      :detail="secretRequest.detail"
      :config="secretRequest.config"
      @done="secretRequest.retry()"
      @close="secretRequest = null"
    />
  </el-container>
</template>

<style scoped>
.startup-error { max-width: 740px; margin: 24px auto; }
.startup-error p { white-space: pre-wrap; overflow-wrap: anywhere; }
.workspace-brand { display: flex; align-items: baseline; gap: 8px; padding: 22px 16px 16px; font-size: 11px; color: var(--el-text-color-secondary); }
.workspace-brand strong { font-size: 20px; color: var(--el-text-color-primary); letter-spacing: -.7px; }
.workspace-brand strong span { color: var(--el-color-primary); }
.navigation-search { padding: 0 12px 14px; }
.nav-empty { padding: 12px 16px; font-size: 12px; color: var(--el-text-color-secondary); }
.welcome { display: flex; flex-direction: column; align-items: flex-start; justify-content: center; max-width: 820px; min-height: 70vh; margin: auto; padding: 24px; box-sizing: border-box; }
.welcome-kicker { color: var(--el-color-primary); font-size: 11px; font-weight: 600; letter-spacing: 3px; }
.welcome h1 { font-size: clamp(30px, 3.2vw, 48px); letter-spacing: -1.6px; margin: 20px 0 12px; }
.welcome p { color: var(--el-text-color-secondary); line-height: 1.8; }
.welcome-actions { display: flex; gap: 10px; flex-wrap: wrap; margin-top: 20px; }
.welcome-actions .el-button { margin-left: 0; }
.welcome-features { display: flex; flex-wrap: wrap; gap: 28px; margin-top: 60px; color: var(--el-text-color-secondary); font-size: 12px; }
.welcome-features b { color: var(--el-color-primary); margin-right: 8px; font-family: monospace; }
.shell {
  height: 100%;
}
.rail {
  width: 56px;
  flex-shrink: 0;
  background: var(--ferry-rail-bg);
  border-right: 1px solid var(--ferry-rail-border);
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 6px;
  padding: 12px 0;
  box-sizing: border-box;
}
.logo {
  width: 32px;
  height: 32px;
  margin-bottom: 10px;
}
.rail-btn {
  width: 40px;
  height: 40px;
  border: 0;
  border-radius: 8px;
  background: transparent;
  color: var(--ferry-rail-icon);
  display: flex;
  align-items: center;
  justify-content: center;
  cursor: pointer;
}
.rail-btn:hover,
.rail-btn.on {
  background: var(--ferry-rail-hover);
  color: var(--ferry-rail-icon-on);
}
.rail-btn:focus-visible {
  outline: 2px solid var(--el-color-primary);
  outline-offset: 1px;
}
.rail-gap {
  flex: 1;
}
.rail-badge :deep(.el-badge__content) {
  transform: translate(60%, -40%) scale(0.85);
}
.aside {
  width: 208px;
  flex-shrink: 0;
  box-sizing: border-box;
  background: var(--el-fill-color-extra-light);
  border-right: 1px solid var(--el-border-color-lighter);
  overflow-y: auto;
  padding: 12px 10px;
}
.aside-title {
  display: flex;
  align-items: center;
  justify-content: space-between;
  min-height: 28px;
  padding: 0 4px 6px 8px;
  font-size: 12px;
  color: var(--el-text-color-secondary);
}
.aside-title.sub {
  padding-top: 14px;
}
.projects {
  list-style: none;
  margin: 0;
  padding: 0;
}
/* 每一项像一张小卡片：带颜色的首字母、名字和第二行说明，之间留空 */
.projects {
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.projects li {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 7px 6px 7px 8px;
  border: 1px solid transparent;
  border-radius: 8px;
  cursor: pointer;
}
.projects li:hover {
  background: var(--el-fill-color-light);
}
.projects li.active {
  background: var(--el-color-primary-light-9);
  border-color: var(--el-color-primary-light-7);
}
.pavatar {
  width: 28px;
  height: 28px;
  flex-shrink: 0;
  border-radius: 7px;
  display: flex;
  align-items: center;
  justify-content: center;
  color: #fff;
  font-size: 13px;
  font-weight: 600;
}
.pavatar.conn {
  position: relative;
  background: var(--el-fill-color-darker);
  color: var(--el-text-color-regular);
  font-size: 15px;
}
.ptext {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 1px;
}
.pname {
  font-size: 13.5px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.projects li.active .pname {
  color: var(--el-color-primary);
  font-weight: 600;
}
.psub {
  font-size: 11.5px;
  color: var(--el-text-color-secondary);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.psub.mono {
  font-size: 11px;
}
.cdot {
  position: absolute;
  right: -2px;
  bottom: -2px;
  width: 9px;
  height: 9px;
  border-radius: 50%;
  border: 2px solid var(--el-bg-color);
}
.cdot.on {
  background: var(--el-color-success);
}
.cdot.connecting {
  background: var(--el-color-warning);
  animation: cdot-blink 1s ease-in-out infinite;
}
@keyframes cdot-blink {
  50% {
    opacity: 0.35;
  }
}
/* 「⋯」只在鼠标移上来、菜单打开时显示；不占名字的位置 */
.pmore {
  visibility: hidden;
  flex-shrink: 0;
}
.projects li:hover .pmore,
.projects li:focus-within .pmore,
.pmore.open {
  visibility: visible;
}
:global(.danger-item) {
  color: var(--el-color-danger) !important;
}
.main {
  min-width: 0;
  padding: 16px 20px;
  overflow: auto;
}
</style>
