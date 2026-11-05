<script setup lang="ts">
// Redis 命令行：像 redis-cli 一样一行一条命令，回车执行，↑↓ 翻以前的命令。同一个窗口用同一条连接（SELECT、MULTI 都保留）。
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue'
import { ElMessageBox } from 'element-plus'
import { Delete, Close } from '@element-plus/icons-vue'
import { closeDbSession, dbRun, formatReply, type DbConn, type RedisCmdResult } from '../../db'

const props = defineProps<{ conn: DbConn; db: string; sid: string }>()
const emit = defineEmits<{ 'update:db': [db: string] }>()

interface Line { k: 'cmd' | 'out' | 'err' | 'info'; t: string }
const lines = ref<Line[]>([])
const cur = ref(Number(props.db || props.conn.database?.replace(/^db/i, '') || 0) || 0)
const input = ref('')
const busy = ref(false)
const out = ref<HTMLElement>()
const inp = ref()
let cancel: (() => void) | null = null

const HIST_KEY = 'ferry.db.redis.history'
let hist: string[] = []
try { hist = JSON.parse(localStorage.getItem(HIST_KEY) || '[]') } catch { /* 没存过 */ }
let hi = hist.length

const prompt = computed(() => `db${cur.value}>`)

function push(k: Line['k'], t: string | string[]) {
  for (const x of Array.isArray(t) ? t : [t]) lines.value.push({ k, t: x })
  if (lines.value.length > 5000) lines.value.splice(0, lines.value.length - 5000)
  void nextTick(() => { if (out.value) out.value.scrollTop = out.value.scrollHeight })
}

onMounted(() => {
  push('info', `${props.conn.name} 的命令行：像 redis-cli 一样输入命令，回车执行；↑↓ 翻以前的命令。${props.conn.readOnly ? '这个连接是只读的，只能执行查看的命令。' : '会删数据、卡住 Redis 的命令（FLUSHDB、KEYS……）执行前会先问你。'}`)
  inp.value?.focus()
})
onBeforeUnmount(() => {
  cancel?.()
  closeDbSession(props.conn.id, props.sid)
})

async function exec(line: string, confirm = false): Promise<void> {
  const r = dbRun<RedisCmdResult>(props.conn, 'redis-cmd', String(cur.value), { session: props.sid, line, confirm })
  cancel = r.cancel
  try {
    const res = await r.done
    if (res.confirm) {
      try {
        await ElMessageBox.confirm(`${res.confirm}。\n\n确定要执行「${line.length > 80 ? line.slice(0, 80) + '…' : line}」吗？`, '这条命令有风险', {
          type: 'warning', confirmButtonText: '执行', cancelButtonText: '不执行', confirmButtonClass: 'el-button--danger',
        })
      } catch {
        push('info', '没有执行')
        return
      }
      return exec(line, true)
    }
    if (res.reply) {
      const l = formatReply(res.reply)
      push(res.reply.t === 'err' ? 'err' : 'out', l)
    }
    if (res.db !== cur.value) {
      cur.value = res.db
      emit('update:db', String(res.db))
    }
  } catch (e) {
    push('err', (e as Error).message)
  } finally {
    cancel = null
  }
}

async function run() {
  const text = input.value
  if (busy.value || !text.trim()) return
  input.value = ''
  busy.value = true
  try {
    // 粘贴了好几行：一行一行执行
    for (const line of text.split(/\r?\n/).filter((l) => l.trim())) {
      push('cmd', `${prompt.value} ${line}`)
      if (hist[hist.length - 1] !== line) hist.push(line)
      if (/^\s*clear\s*$/i.test(line)) {
        lines.value = []
        continue
      }
      await exec(line)
    }
    hist = hist.slice(-200)
    hi = hist.length
    try { localStorage.setItem(HIST_KEY, JSON.stringify(hist)) } catch { /* 存不下就算了 */ }
  } finally {
    busy.value = false
    void nextTick(() => inp.value?.focus())
  }
}

function onKey(e: KeyboardEvent) {
  if (e.key === 'Enter' && !e.shiftKey) {
    e.preventDefault()
    void run()
  } else if (e.key === 'ArrowUp' && !input.value.includes('\n')) {
    if (hi > 0) input.value = hist[--hi]
    e.preventDefault()
  } else if (e.key === 'ArrowDown' && !input.value.includes('\n')) {
    hi = Math.min(hist.length, hi + 1)
    input.value = hist[hi] ?? ''
    e.preventDefault()
  }
}
function stop() {
  cancel?.()
  push('info', '已取消（Redis 那边可能还在执行）')
}
</script>

<template>
  <div class="rc">
    <div class="bar">
      <span class="title">命令行 · {{ conn.name }}</span>
      <span class="grow" />
      <el-button v-if="busy" size="small" :icon="Close" @click="stop">取消</el-button>
      <el-button size="small" :icon="Delete" @click="lines = []">清屏</el-button>
    </div>
    <div ref="out" class="out" @click="inp?.focus()">
      <div v-for="(l, i) in lines" :key="i" class="ln" :class="l.k">{{ l.t || ' ' }}</div>
      <div v-if="busy" class="ln info">执行中…</div>
    </div>
    <div class="in">
      <span class="pr">{{ prompt }}</span>
      <el-input ref="inp" v-model="input" type="textarea" :autosize="{ minRows: 1, maxRows: 6 }" resize="none" class="cmd" spellcheck="false" placeholder="如 GET user:1、HGETALL session:abc、INFO memory" @keydown="onKey" />
    </div>
  </div>
</template>

<style scoped>
.rc {
  height: 100%;
  display: flex;
  flex-direction: column;
  min-height: 0;
}
.bar {
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 8px 10px;
  border-bottom: 1px solid var(--el-border-color-lighter);
}
.bar .el-button + .el-button {
  margin-left: 0;
}
.title {
  font-size: 13px;
  color: var(--el-text-color-secondary);
}
.grow {
  flex: 1;
}
.out {
  flex: 1;
  min-height: 0;
  overflow: auto;
  padding: 8px 12px;
  font-family: 'Cascadia Mono', Consolas, monospace;
  font-size: 12.5px;
  line-height: 1.55;
  background: var(--el-fill-color-extra-light);
  user-select: text;
}
.ln {
  white-space: pre-wrap;
  word-break: break-all;
}
.ln.cmd {
  color: var(--el-color-primary);
  margin-top: 6px;
}
.ln.err {
  color: var(--el-color-danger);
}
.ln.info {
  color: var(--el-text-color-secondary);
}
.in {
  display: flex;
  align-items: flex-start;
  gap: 6px;
  padding: 8px 10px;
  border-top: 1px solid var(--el-border-color-lighter);
}
.pr {
  font-family: 'Cascadia Mono', Consolas, monospace;
  font-size: 12.5px;
  line-height: 28px;
  color: var(--el-color-primary);
  flex-shrink: 0;
}
.cmd :deep(textarea) {
  font-family: 'Cascadia Mono', Consolas, monospace;
  font-size: 12.5px;
}
</style>
