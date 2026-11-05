<script setup lang="ts">
import { computed, ref } from 'vue'
import type Node from 'element-plus/es/components/tree/src/model/node'
import { Document, Folder, Link } from '@element-plus/icons-vue'
import { bytes, type KeepEntry, type KeepItem, type KeepReport } from '../ferry'
import { readCall } from '../readRequests'

// 从服务器上运行中的版本里选部署时要保留的文件、目录
const props = defineProps<{ config: string; target: string; app: string; type: string; program?: string; selected: string[] }>()
const emit = defineEmits<{ close: []; pick: [items: KeepItem[]] }>()

const visible = ref(true)
const error = ref('')
const release = ref<string>()
const loaded = ref(false)
const tree = ref<{ getCheckedNodes: () => KeepEntry[] }>()
const checked = ref<KeepEntry[]>([])
const ctl = new AbortController()

// 不能保留的：Ferry 自己的、运行日志、服务器上装的依赖、程序文件
const reserved = computed(() => {
  const set = new Set(['.ferry'])
  if (props.type !== 'static') ['logs', '.venv', 'node_modules'].forEach((n) => set.add(n))
  if (props.program) set.add(props.program)
  return set
})
// el-tree 也拿根节点（没有 path）来问
const disabled = (d: KeepEntry) => !!d?.path && !d.path.includes('/') && reserved.value.has(d.name)
const filler = (d: KeepEntry) => !!d?.path?.includes('\u0000')
const treeProps = { label: 'name', isLeaf: (d: KeepEntry) => !d?.dir || !!d?.link, disabled: (d: KeepEntry) => disabled(d) || filler(d) }

async function load(node: Node, resolve: (data: KeepEntry[]) => void) {
  const dir = node.level === 0 ? '.' : (node.data as KeepEntry).path
  try {
    const r = await readCall<KeepReport>(['keep', 'scan', props.target, props.app, '--dir', dir, '--keep-files', '[]', '-c', props.config], ctl.signal)
    if (node.level === 0) {
      release.value = r.release
      loaded.value = true
    }
    const list = r.entries ?? []
    if (r.more) list.push({ name: '…目录里东西太多，只列了一部分', path: dir + '/\u0000more', size: 0 })
    resolve(list)
  } catch (e) {
    if (node.level === 0) {
      loaded.value = true
      error.value = (e as Error).message
    }
    resolve([])
  }
}

function onCheck() {
  checked.value = (tree.value?.getCheckedNodes() ?? []).filter((x) => !filler(x))
}

function confirm() {
  // 目录默认共用（上传目录这类），文件默认随版本带过去；勾了目录又勾了它下面的，只要目录
  const picked = checked.value.filter((x) => !checked.value.some((y) => y !== x && y.dir && x.path.startsWith(y.path + '/')))
  emit('pick', picked.map((x) => ({ path: x.path, shared: !!x.dir && !x.link })))
  visible.value = false
}

const size = (d: KeepEntry) => (d.dir ? '' : bytes(d.size ?? 0))
</script>

<template>
  <el-dialog v-model="visible" :title="`从服务器选要保留的：${app}`" width="560px" append-to-body @closed="ctl.abort(); emit('close')">
    <div class="tip muted">
      {{ target }} 上运行中的版本{{ release ? ` ${release}` : '' }}里的文件。勾选的部署时用服务器上的，不会被包里的替换。
    </div>
    <div v-if="loaded && !error && !release" class="muted">{{ app }} 在 {{ target }} 上还没部署过，没有可选的。可以先部署一次，或在 ferry.yaml 的 artifact.keep 里直接写路径。</div>
    <div v-if="error" class="err">{{ error }}</div>
    <div class="tree-box">
      <el-tree
        ref="tree"
        lazy
        show-checkbox
        check-strictly
        node-key="path"
        :load="load"
        :default-checked-keys="selected"
        :props="treeProps"
        @check="onCheck"
      >
        <template #default="{ data }">
          <span class="node" :class="{ off: disabled(data) }" :title="disabled(data) ? '不能保留（Ferry 自己用的、运行日志、依赖或程序文件）' : data.path">
            <el-icon><component :is="data.link ? Link : data.dir ? Folder : Document" /></el-icon>
            <span>{{ data.name }}</span>
            <span v-if="data.link" class="muted small">链接</span>
            <span class="muted small">{{ size(data) }}</span>
          </span>
        </template>
      </el-tree>
    </div>
    <template #footer>
      <div class="foot">
        <span class="muted small">目录默认「共用」（只有一份，回滚时不动），添加后可以改</span>
        <el-button @click="visible = false">取消</el-button>
        <el-button type="primary" :disabled="!checked.length" @click="confirm">添加 {{ checked.length || '' }}</el-button>
      </div>
    </template>
  </el-dialog>
</template>

<style scoped>
.tip { font-size: 12px; line-height: 1.6; margin-bottom: 8px; }
.err { color: var(--el-color-danger); font-size: 12px; margin-bottom: 8px; }
.tree-box { max-height: 52vh; overflow: auto; border: 1px solid var(--ferry-stroke); border-radius: 6px; padding: 4px 0; min-height: 120px; }
.node { display: inline-flex; align-items: center; gap: 6px; font-size: 13px; }
.node.off { color: var(--el-text-color-disabled); }
.small { font-size: 11px; }
.foot { display: flex; align-items: center; gap: 8px; }
.foot > span { margin-right: auto; text-align: left; }
</style>
