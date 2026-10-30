<script setup lang="ts">
// 终端窗口：停靠在主界面下方，可以开多个标签、拖动上边调高度、铺满窗口、收起（收起后终端保持连接）。
import { computed, defineAsyncComponent, onBeforeUnmount, ref } from 'vue'
import { ElMessageBox } from 'element-plus'
import { Close, FullScreen, Minus, Monitor, Plus, ScaleToOriginal } from '@element-plus/icons-vue'
import { activeTerminal, addTerminal, removeTerminal, terminalHeight, terminalShown, terminals, type TermTab } from '../state'

// 用到时才加载（编辑器、终端、AI 助手和各个对话框不放进启动时加载的代码里）
const TerminalView = defineAsyncComponent(() => import('./TerminalView.vue'))

const full = ref(false)
const current = computed(() => terminals.find((t) => t.id === activeTerminal.value))

async function close(t: TermTab) {
  if (t.state === 'open') {
    try {
      await ElMessageBox.confirm(`关闭 ${t.title} 的终端？里面正在运行的命令会被结束。`, '关闭终端', { type: 'warning', confirmButtonText: '关闭' })
    } catch {
      return
    }
  }
  removeTerminal(t.id)
}

function another() {
  if (current.value) addTerminal(current.value.config, current.value.target)
}

// 拖动上边调整高度
let drag: { y: number; h: number } | undefined
function startDrag(e: MouseEvent) {
  if (full.value) return
  drag = { y: e.clientY, h: terminalHeight.value }
  window.addEventListener('mousemove', onDrag)
  window.addEventListener('mouseup', stopDrag)
  document.body.style.userSelect = 'none'
}
function onDrag(e: MouseEvent) {
  if (!drag) return
  terminalHeight.value = Math.round(Math.min(Math.max(drag.h + drag.y - e.clientY, 160), window.innerHeight - 60))
}
function stopDrag() {
  drag = undefined
  window.removeEventListener('mousemove', onDrag)
  window.removeEventListener('mouseup', stopDrag)
  document.body.style.userSelect = ''
}
onBeforeUnmount(stopDrag)
</script>

<template>
  <div v-show="terminalShown && terminals.length" class="term-host" :class="{ full }" :style="full ? {} : { height: terminalHeight + 'px' }">
    <div class="drag" @mousedown="startDrag" />
    <div class="term-head">
      <div class="tabs">
        <div
          v-for="t in terminals"
          :key="t.id"
          class="tab"
          :class="{ active: t.id === activeTerminal }"
          @click="activeTerminal = t.id"
          @mousedown.middle.prevent="close(t)"
        >
          <span class="led" :class="t.state" />
          <span class="title">{{ t.title }}</span>
          <el-icon class="x" @click.stop="close(t)"><Close /></el-icon>
        </div>
        <el-tooltip :show-after="HOVER_DELAY" content="再开一个终端，连到同一台服务器" placement="bottom">
          <el-button text size="small" :icon="Plus" class="head-btn" @click="another" />
        </el-tooltip>
      </div>
      <el-tooltip :show-after="HOVER_DELAY" :content="full ? '还原' : '铺满窗口'" placement="bottom">
        <el-button text size="small" :icon="full ? ScaleToOriginal : FullScreen" class="head-btn" @click="full = !full" />
      </el-tooltip>
      <el-tooltip :show-after="HOVER_DELAY" content="收起（终端保持连接，点右下角的「终端」再打开）" placement="bottom">
        <el-button text size="small" :icon="Minus" class="head-btn" @click="terminalShown = false" />
      </el-tooltip>
    </div>
    <div class="term-body">
      <TerminalView
        v-for="t in terminals"
        v-show="t.id === activeTerminal"
        :key="t.id"
        :tab="t"
        :active="terminalShown && t.id === activeTerminal"
      />
    </div>
  </div>
  <div v-if="!terminalShown && terminals.length" class="term-pill" @click="terminalShown = true">
    <el-icon><Monitor /></el-icon>
    终端（{{ terminals.length }}）
  </div>
</template>

<style scoped>
.term-host {
  position: fixed;
  left: var(--ferry-side, 264px);
  right: 0;
  bottom: 0;
  z-index: 1500;
  max-height: calc(100vh - 40px);
  display: flex;
  flex-direction: column;
  background: #1e1e1e;
  border-top: 1px solid #333;
  box-shadow: 0 -4px 16px rgba(0, 0, 0, 0.25);
}
.term-host.full {
  left: 0;
  top: 0;
  max-height: none;
}
.drag {
  position: absolute;
  top: -3px;
  left: 0;
  right: 0;
  height: 6px;
  cursor: ns-resize;
  z-index: 1;
}
.full .drag {
  display: none;
}
.term-head {
  display: flex;
  align-items: center;
  height: 32px;
  padding: 0 6px;
  background: #252526;
  color: #ccc;
}
.tabs {
  flex: 1;
  display: flex;
  align-items: center;
  gap: 2px;
  min-width: 0;
  overflow-x: auto;
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
  white-space: nowrap;
  color: #aaa;
}
.tab:hover {
  background: #2d2d2d;
}
.tab.active {
  background: #1e1e1e;
  color: #fff;
}
.tab .x {
  font-size: 12px;
  border-radius: 3px;
  opacity: 0.6;
}
.tab .x:hover {
  opacity: 1;
  background: #444;
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
.head-btn {
  color: #ccc;
}
.term-body {
  flex: 1;
  min-height: 0;
}
.term-pill {
  position: fixed;
  right: 16px;
  bottom: 16px;
  z-index: 1500;
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 6px 14px;
  border-radius: 16px;
  background: #252526;
  color: #eee;
  font-size: 13px;
  cursor: pointer;
  box-shadow: 0 2px 10px rgba(0, 0, 0, 0.3);
}
.term-pill:hover {
  background: #333;
}
</style>
