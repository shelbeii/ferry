<script setup lang="ts">
// 设置 → 通知：部署、回滚结束后发到企业微信、钉钉、飞书的群机器人，或自己的接口。
// 机器人地址（含令牌）和加签密钥存在系统凭据库，按 ID 区分；改动在设置「保存」时才写入（applySecrets）。
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Plus } from '@element-plus/icons-vue'
import { call, NOTIFY_KINDS, type AppSettings, type NotifyChannel } from '../ferry'
import { projects } from '../state'
import GlassSwitch from './glass/GlassSwitch.vue'
import GlassToggle from './glass/GlassToggle.vue'

const props = defineProps<{ notify: AppSettings['notify'] }>()

const stored = ref<Record<string, { url: boolean; sign: boolean }>>({})
const envCount = ref(0)
// 保存时写入 / 删除：url:<id>、sign:<id>
const pending = reactive<Record<string, string>>({})
const pendingDeletes = reactive(new Set<string>())

async function loadStored() {
  try {
    const r = await call<{ channels: (NotifyChannel & { url: boolean; sign: boolean })[]; env: number }>(['notify', 'list'])
    stored.value = Object.fromEntries(r.channels.map((c) => [c.id, { url: c.url, sign: c.sign }]))
    envCount.value = r.env
  } catch {
    stored.value = {}
  }
}
onMounted(loadStored)

const has = (what: 'url' | 'sign', id: string) =>
  !!pending[`${what}:${id}`] || (!!stored.value[id]?.[what] && !pendingDeletes.has(`${what}:${id}`))

const hint: Record<NotifyChannel['kind'], string> = {
  wecom: '群聊 → 群机器人 → 添加机器人，复制 Webhook 地址（https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=…）',
  dingtalk: '群设置 → 机器人 → 添加机器人 → 自定义，复制 Webhook 地址。安全设置选「加签」就把密钥填在下面；选「自定义关键词」就加上「部署」',
  feishu: '群设置 → 群机器人 → 添加机器人 → 自定义机器人，复制 Webhook 地址。设了签名校验就把密钥填在下面；设了关键词就加上「部署」',
  webhook: 'Ferry 用 POST 发 JSON：项目、服务器、成功与否、版本、提交、操作人、各应用的结果，以及现成的 title、markdown',
}
const placeholder: Record<NotifyChannel['kind'], string> = {
  wecom: 'https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=…',
  dingtalk: 'https://oapi.dingtalk.com/robot/send?access_token=…',
  feishu: 'https://open.feishu.cn/open-apis/bot/v2/hook/…',
  webhook: 'https://…',
}

function describe(c: NotifyChannel): string {
  const parts: string[] = []
  const on = c.on ?? []
  parts.push(on.length === 1 ? (on[0] === 'success' ? '只发成功的' : '只发失败的') : '成功、失败都发')
  if (c.productionOnly) parts.push('只发生产服务器')
  if (c.scope?.length) parts.push(`只发 ${c.scope.join('、')}`)
  return parts.join(' · ')
}

// ---------------------------------------------------------------- 编辑

const editing = ref(false)
const editIndex = ref(-1)
const blank = (): NotifyChannel => ({ id: '', name: '', kind: 'dingtalk', on: [], scope: [], productionOnly: false, off: false })
const draft = reactive<NotifyChannel>(blank())
const draftURL = ref('')
const draftSign = ref('')
const testing = ref(false)
const testResult = ref<{ ok: boolean; msg: string }>()
// 「成功」「失败」两个开关：都开 = 空列表（都发）
const draftOn = computed({
  get: () => (draft.on?.length ? draft.on : ['success', 'failure']),
  set: (v: string[]) => (draft.on = v.length === 2 ? [] : v),
})
function setOn(kind: 'success' | 'failure', on: boolean) {
  const rest = draftOn.value.filter((k) => k !== kind)
  draftOn.value = on ? [...rest, kind] : rest
}
const projectNames = computed(() => [...new Set(projects.value.map((p) => p.name))])

function newID(): string {
  const taken = new Set(props.notify.channels.map((c) => c.id))
  let i = props.notify.channels.length + 1
  while (taken.has(`n${i}`)) i++
  return `n${i}`
}

function openEdit(i: number) {
  editIndex.value = i
  const c = i >= 0 ? props.notify.channels[i] : undefined
  Object.assign(draft, blank(), c ? { ...c, on: [...(c.on ?? [])], scope: [...(c.scope ?? [])] } : {})
  draftURL.value = ''
  draftSign.value = ''
  testResult.value = undefined
  editing.value = true
}

function saveEdit() {
  const url = draftURL.value.trim()
  if (url && !/^https?:\/\//.test(url)) {
    ElMessage.warning('地址要以 https:// 开头')
    return
  }
  if (editIndex.value < 0 && !url) {
    ElMessage.warning('填上机器人的 Webhook 地址')
    return
  }
  if (draftOn.value.length === 0) {
    ElMessage.warning('成功、失败至少打开一个')
    return
  }
  draft.name = draft.name.trim() || NOTIFY_KINDS[draft.kind]
  if (editIndex.value < 0) {
    draft.id = newID()
    props.notify.channels.push({ ...draft })
  } else {
    props.notify.channels[editIndex.value] = { ...draft }
  }
  for (const [what, v] of [
    ['url', url],
    ['sign', draftSign.value.trim()],
  ] as const) {
    if (v) {
      pending[`${what}:${draft.id}`] = v
      pendingDeletes.delete(`${what}:${draft.id}`)
    }
  }
  editing.value = false
}

async function remove(i: number) {
  const c = props.notify.channels[i]
  try {
    await ElMessageBox.confirm(`删除「${c.name}」？它的地址、加签密钥也会从凭据库删除（点设置的「保存」后生效）。`, '删除通知方式', { type: 'warning' })
  } catch {
    return
  }
  props.notify.channels.splice(i, 1)
  for (const what of ['url', 'sign']) {
    delete pending[`${what}:${c.id}`]
    pendingDeletes.add(`${what}:${c.id}`)
  }
}

function deleteDraftSign() {
  delete pending[`sign:${draft.id}`]
  pendingDeletes.add(`sign:${draft.id}`)
  draftSign.value = ''
}

// 用编辑框里（还没保存）的值测试
async function test() {
  testing.value = true
  testResult.value = undefined
  const env: Record<string, string> = { FERRY_NOTIFY_TEST_KIND: draft.kind }
  const url = draftURL.value.trim() || (draft.id ? pending[`url:${draft.id}`] : '')
  const sign = draftSign.value.trim() || (draft.id ? pending[`sign:${draft.id}`] : '')
  if (url) env.FERRY_NOTIFY_TEST_URL = url
  if (sign) env.FERRY_NOTIFY_TEST_SIGN = sign
  else if (draft.id && pendingDeletes.has(`sign:${draft.id}`)) env.FERRY_NOTIFY_TEST_NO_SIGN = '1'
  const saved = draft.id && stored.value[draft.id]?.url && !pendingDeletes.has(`url:${draft.id}`)
  try {
    await call(['notify', 'test', ...(saved ? [draft.id] : [])], env)
    testResult.value = { ok: true, msg: '已发送，去群里看看收到没有' }
  } catch (e) {
    testResult.value = { ok: false, msg: (e as Error).message }
  } finally {
    testing.value = false
  }
}

/** 设置保存时调用：写入新填的地址、加签密钥，删除删掉的 */
async function applySecrets() {
  for (const key of pendingDeletes) {
    if (pending[key]) continue
    const [what, id] = key.split(':')
    await call(['notify', 'forget', id, ...(what === 'sign' ? ['--sign'] : [])])
  }
  for (const [key, v] of Object.entries(pending)) {
    const [what, id] = key.split(':')
    await call(['notify', 'save', id, ...(what === 'sign' ? ['--sign'] : [])], { FERRY_SECRET_VALUE: v })
  }
  pendingDeletes.clear()
  for (const k of Object.keys(pending)) delete pending[k]
}
defineExpose({ applySecrets })
</script>

<template>
  <div class="channels">
    <div v-if="!notify.channels.length" class="muted small empty">还没有。添加一个群机器人，部署、回滚结束后就会在群里通知结果。</div>
    <div v-for="(c, i) in notify.channels" :key="c.id" class="channel" :class="{ off: c.off }">
      <GlassSwitch :model-value="!c.off" size="small" :aria-label="`启用 ${c.name}`" @change="(v: boolean) => (c.off = !v)" />
      <span class="name">{{ c.name }}</span>
      <el-tag size="small" effect="plain" disable-transitions>{{ NOTIFY_KINDS[c.kind] }}</el-tag>
      <span class="muted small meta">{{ describe(c) }}</span>
      <el-tag v-if="!has('url', c.id)" size="small" type="warning" effect="plain" disable-transitions>没有地址</el-tag>
      <span class="spacer" />
      <el-button text size="small" @click="openEdit(i)">编辑</el-button>
      <el-button text size="small" type="danger" @click="remove(i)">删除</el-button>
    </div>
    <div class="add">
      <el-button size="small" :icon="Plus" @click="openEdit(-1)">添加</el-button>
    </div>
    <p v-if="envCount" class="muted small">另有 {{ envCount }} 个来自环境变量 FERRY_NOTIFY。</p>

    <el-dialog v-model="editing" :title="editIndex < 0 ? '添加通知' : '编辑通知'" width="580px" append-to-body>
      <el-form label-width="112px">
        <el-form-item label="发到">
          <el-radio-group v-model="draft.kind">
            <el-radio-button v-for="(label, k) in NOTIFY_KINDS" :key="k" :value="k">{{ label }}</el-radio-button>
          </el-radio-group>
          <div class="muted small tip">{{ hint[draft.kind] }}</div>
        </el-form-item>
        <el-form-item label="名称">
          <el-input v-model="draft.name" :placeholder="`如「后端群」，不填 = ${NOTIFY_KINDS[draft.kind]}`" />
        </el-form-item>
        <el-form-item label="Webhook 地址">
          <el-input
            v-model="draftURL"
            type="password"
            show-password
            class="mono"
            :placeholder="draft.id && has('url', draft.id) ? '已保存（填写则替换）' : placeholder[draft.kind]"
          />
          <div class="muted small tip">地址里有令牌，存在系统凭据库，不写进设置文件</div>
        </el-form-item>
        <el-form-item v-if="draft.kind === 'dingtalk' || draft.kind === 'feishu'" label="加签密钥">
          <div class="row">
            <el-input
              v-model="draftSign"
              type="password"
              show-password
              class="mono"
              :placeholder="draft.id && has('sign', draft.id) ? '已保存（填写则替换）' : '机器人设了加签 / 签名校验才填'"
            />
            <el-button v-if="draft.id && has('sign', draft.id)" @click="deleteDraftSign">删除</el-button>
          </div>
        </el-form-item>
        <el-form-item label="什么时候发">
          <div class="when">
            <GlassToggle v-for="w in (['success', 'failure'] as const)" :key="w" :model-value="draftOn.includes(w)" @update:model-value="(v: boolean) => setOn(w, v)">
              {{ w === 'success' ? '部署 / 回滚成功' : '失败' }}
            </GlassToggle>
          </div>
        </el-form-item>
        <el-form-item label="哪些项目">
          <el-select v-model="draft.scope" multiple filterable allow-create clearable placeholder="全部项目" style="width: 100%">
            <el-option v-for="n in projectNames" :key="n" :label="n" :value="n" />
          </el-select>
          <GlassToggle v-model="draft.productionOnly" class="prod">只发生产服务器（production: true）的</GlassToggle>
        </el-form-item>
        <el-form-item>
          <el-button :loading="testing" @click="test">发一条测试消息</el-button>
          <span v-if="testResult" class="small ml" :class="testResult.ok ? 'ok' : 'bad'">{{ testResult.msg }}</span>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="editing = false">取消</el-button>
        <el-button type="primary" @click="saveEdit">确定</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.channels {
  margin: 4px 0 8px;
}
.empty {
  padding: 6px 0;
}
.channel {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 4px 8px;
  border: 1px solid var(--el-border-color-lighter);
  border-radius: 6px;
  margin-bottom: 6px;
}
.channel.off .name,
.channel.off .meta {
  opacity: 0.55;
}
.name {
  font-weight: 600;
}
.meta {
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.spacer {
  flex: 1;
}
.add {
  display: flex;
  align-items: center;
  gap: 10px;
}
.small {
  font-size: 12px;
}
.tip {
  line-height: 1.6;
  margin-top: 4px;
}
.ml {
  margin-left: 8px;
}
.row {
  display: flex;
  gap: 8px;
  width: 100%;
}
.prod {
  margin-top: 6px;
}
.when {
  display: flex;
  flex-wrap: wrap;
  gap: 6px 24px;
}
.ok {
  color: var(--el-color-success);
}
.bad {
  color: var(--el-color-danger);
}
</style>
