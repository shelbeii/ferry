<script setup lang="ts">
// 设置 → AI 的模型列表：可以配置多个模型（各自的服务商、地址、模型名、API Key、默认思考强度），选一个作为默认。
// API Key 存在系统凭据库，按模型 ID 区分；改动在设置「保存」时才写入（applyKeys）。
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Plus } from '@element-plus/icons-vue'
import { call, type AiModel, type AppSettings } from '../ferry'
import { EFFORT_LABELS, THINKING_STYLES, effortOptions, plainHttpOutside, supportsImages, thinkingStyle } from '../ai'

const props = defineProps<{ ai: AppSettings['ai'] }>()

const keyStored = ref<Record<string, boolean>>({})
const pendingKeys = reactive<Record<string, string>>({}) // 保存时写入
const pendingDeletes = reactive(new Set<string>()) // 保存时删除

async function loadKeys() {
  try {
    keyStored.value = (await call<{ models: Record<string, boolean> }>(['ai-key', 'status'])).models ?? {}
  } catch {
    keyStored.value = {}
  }
}
onMounted(loadKeys)

const hasKey = (id: string) => !!pendingKeys[id] || (!!keyStored.value[id] && !pendingDeletes.has(id))

const providerLabel = (p: string) => (p === 'openai' ? 'OpenAI 兼容' : 'Anthropic')

// ---------------------------------------------------------------- 编辑

const editing = ref(false)
const editIndex = ref(-1) // -1 = 新加
const blank = (): AiModel => ({
  id: '',
  name: '',
  provider: 'openai',
  baseUrl: '',
  model: '',
  effort: '',
  thinking: '',
  images: '',
  priceIn: 0,
  priceCache: 0,
  priceOut: 0,
  currency: '¥',
})
const draft = reactive<AiModel>(blank())
const draftKey = ref('')
const testing = ref(false)
const testResult = ref<{ ok: boolean; msg: string }>()

const draftEfforts = computed(() => effortOptions(draft))
const autoStyle = computed(() => thinkingStyle({ ...draft, thinking: '' }))
const autoImages = computed(() => supportsImages({ ...draft, images: '' }))
// 下拉框里「自动识别」是 'auto'，存的是空字符串
const draftImages = computed({
  get: () => draft.images || 'auto',
  set: (v: string) => (draft.images = v === 'auto' ? '' : v),
})
// 下拉框里「不设置」是 'none'，存的是空字符串
const draftEffort = computed({
  get: () => draft.effort || 'none',
  set: (v: string) => (draft.effort = v === 'none' ? '' : v),
})

function newID(base: string): string {
  let s = base
    .toLowerCase()
    .replace(/[^a-z0-9-]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 24)
  if (!/^[a-z0-9]/.test(s)) s = 'm' + Date.now().toString(36).slice(-4)
  const taken = new Set(props.ai.models.map((m) => m.id))
  let id = s
  for (let i = 2; taken.has(id); i++) id = `${s}-${i}`
  return id
}

function openEdit(i: number) {
  editIndex.value = i
  Object.assign(draft, blank(), i >= 0 ? props.ai.models[i] : {})
  draftKey.value = ''
  testResult.value = undefined
  editing.value = true
}

function saveEdit() {
  draft.name = draft.name.trim()
  draft.model = draft.model.trim()
  draft.baseUrl = draft.baseUrl.trim()
  if (draft.provider === 'openai' && (!draft.baseUrl || !draft.model)) {
    ElMessage.warning('OpenAI 兼容接口要填接口地址和模型')
    return
  }
  if (!draftEfforts.value.some((o) => o.value === (draft.effort || 'none'))) draft.effort = '' // 换了服务商或思考参数，没有这一档
  if (draft.provider !== 'openai') draft.thinking = ''
  if (editIndex.value < 0) {
    draft.id = newID(draft.model || draft.name || 'model')
    props.ai.models.push({ ...draft, name: draft.name || draft.model || draft.id })
    if (!props.ai.default) props.ai.default = draft.id
  } else {
    props.ai.models[editIndex.value] = { ...draft, name: draft.name || draft.model || draft.id }
  }
  if (draftKey.value.trim()) {
    pendingKeys[draft.id] = draftKey.value.trim()
    pendingDeletes.delete(draft.id)
  }
  editing.value = false
}

async function remove(i: number) {
  const m = props.ai.models[i]
  try {
    await ElMessageBox.confirm(`删除模型「${m.name}」？它的 API Key 也会从凭据库删除（点设置的「保存」后生效）。`, '删除模型', { type: 'warning' })
  } catch {
    return
  }
  props.ai.models.splice(i, 1)
  delete pendingKeys[m.id]
  pendingDeletes.add(m.id)
  if (props.ai.default === m.id) props.ai.default = props.ai.models[0]?.id ?? ''
}

function deleteDraftKey() {
  delete pendingKeys[draft.id]
  pendingDeletes.add(draft.id)
  draftKey.value = ''
}

// 用编辑框里（还没保存）的值测试
async function test() {
  testing.value = true
  testResult.value = undefined
  const env: Record<string, string> = {
    FERRY_AI_PROVIDER: draft.provider,
    FERRY_AI_BASE_URL: draft.baseUrl.trim(),
    FERRY_AI_MODEL: draft.model.trim(),
    FERRY_AI_EFFORT: draft.effort,
    FERRY_AI_THINKING: draft.thinking,
  }
  const key = draftKey.value.trim() || (draft.id ? pendingKeys[draft.id] : '')
  if (key) env.FERRY_AI_KEY = key
  else if (draft.id && !pendingDeletes.has(draft.id)) env.FERRY_AI_MODEL_ID = draft.id
  try {
    const r = await call<{ reply: string; model: string; seconds: number }>(['ai-test'], env)
    testResult.value = { ok: true, msg: `${r.model} 回复「${r.reply}」，用时 ${r.seconds.toFixed(1)} 秒` }
  } catch (e) {
    testResult.value = { ok: false, msg: (e as Error).message }
  } finally {
    testing.value = false
  }
}

/** 设置保存时调用：写入新填的 API Key、删除删掉的模型的 Key */
async function applyKeys() {
  for (const id of pendingDeletes) {
    if (!pendingKeys[id]) await call(['ai-key', 'delete', '--model', id])
  }
  for (const [id, v] of Object.entries(pendingKeys)) {
    await call(['ai-key', 'set', '--model', id], { FERRY_SECRET_VALUE: v })
  }
  pendingDeletes.clear()
  for (const k of Object.keys(pendingKeys)) delete pendingKeys[k]
}
defineExpose({ applyKeys })
</script>

<template>
  <div class="models">
    <div v-if="!ai.models.length" class="muted small empty">还没有模型。添加一个，项目和连接里的「AI 助手」就能用了。</div>
    <div v-for="(m, i) in ai.models" :key="m.id" class="model" :class="{ def: ai.default === m.id }">
      <el-radio :model-value="ai.default" :value="m.id" @change="ai.default = m.id">
        <span class="name">{{ m.name }}</span>
      </el-radio>
      <span class="muted small meta">{{ providerLabel(m.provider) }} · <span class="mono">{{ m.model || 'claude-opus-5-5' }}</span></span>
      <el-tag v-if="m.effort" size="small" effect="plain" disable-transitions>思考：{{ EFFORT_LABELS[m.effort] }}</el-tag>
      <el-tag v-if="supportsImages(m)" size="small" effect="plain" disable-transitions>能看图片</el-tag>
      <el-tag v-if="m.priceIn || m.priceOut" size="small" effect="plain" type="info" disable-transitions>
        {{ m.currency }}{{ m.priceIn }} / {{ m.currency }}{{ m.priceOut }}
      </el-tag>
      <el-tag v-if="!hasKey(m.id)" size="small" type="warning" effect="plain" disable-transitions>没有 API Key</el-tag>
      <span class="spacer" />
      <el-button text size="small" @click="openEdit(i)">编辑</el-button>
      <el-button text size="small" type="danger" @click="remove(i)">删除</el-button>
    </div>
    <div class="add">
      <el-button size="small" :icon="Plus" @click="openEdit(-1)">添加模型</el-button>
      <span v-if="ai.models.length > 1" class="muted small">选中的是默认模型；对话时可以换。</span>
    </div>

    <el-dialog v-model="editing" :title="editIndex < 0 ? '添加模型' : '编辑模型'" width="560px" append-to-body>
      <el-form label-width="100px">
        <el-form-item label="名称">
          <el-input v-model="draft.name" placeholder="对话时显示，如 DeepSeek、Claude" />
        </el-form-item>
        <el-form-item label="服务商">
          <el-radio-group v-model="draft.provider">
            <el-radio value="anthropic">Anthropic（Claude）</el-radio>
            <el-radio value="openai">OpenAI 兼容接口</el-radio>
          </el-radio-group>
          <div v-if="draft.provider === 'openai'" class="muted small">DeepSeek、通义千问、Kimi，或内网用 vLLM、Ollama 部署的模型（模型要支持工具调用）</div>
        </el-form-item>
        <el-form-item label="接口地址">
          <el-input v-model="draft.baseUrl" class="mono" :placeholder="draft.provider === 'openai' ? '如 https://api.deepseek.com/v1' : '不填 = 官方地址'" />
          <div v-if="plainHttpOutside(draft.baseUrl)" class="warn-line small">
            这是 http（不加密）的外部地址：API Key 和对话内容会明文传输，能用 https 就用 https
          </div>
        </el-form-item>
        <el-form-item label="模型">
          <el-input v-model="draft.model" class="mono" :placeholder="draft.provider === 'openai' ? '如 deepseek-chat、qwen-plus' : '不填 = claude-opus-5-5'" />
        </el-form-item>
        <el-form-item label="API Key">
          <div class="row">
            <el-input
              v-model="draftKey"
              type="password"
              show-password
              class="mono"
              :placeholder="draft.id && hasKey(draft.id) ? '已保存（填写则替换）' : '保存在系统凭据库'"
            />
            <el-button v-if="draft.id && hasKey(draft.id)" @click="deleteDraftKey">删除</el-button>
          </div>
        </el-form-item>
        <el-form-item v-if="draft.provider === 'openai'" label="思考参数">
          <el-select v-model="draft.thinking" style="width: 260px">
            <el-option v-for="o in THINKING_STYLES" :key="o.value" :value="o.value" :label="o.value ? o.label : `${o.label}（${THINKING_STYLES.find((x) => x.value === autoStyle)?.label}）`" />
          </el-select>
          <div class="muted small">各家开关思考的参数不一样；不确定就用「自动识别」，报错时改成「不传思考参数」</div>
        </el-form-item>
        <el-form-item label="思考强度">
          <el-select v-model="draftEffort" style="width: 160px">
            <el-option v-for="o in draftEfforts" :key="o.value" :label="o.label" :value="o.value" />
          </el-select>
          <span class="muted small ml">对话时的默认值，可以另选</span>
        </el-form-item>
        <el-form-item label="图片">
          <el-select v-model="draftImages" style="width: 200px">
            <el-option value="auto" :label="`自动识别（${autoImages ? '能看' : '不能看'}）`" />
            <el-option value="on" label="能看图片" />
            <el-option value="off" label="不能看图片" />
          </el-select>
          <div class="muted small">能看图片的模型，对话时可以贴截图、附图片；按模型名识别不准时在这里改</div>
        </el-form-item>
        <el-form-item label="单价">
          <div class="prices">
            <span class="muted small">输入</span>
            <el-input-number v-model="draft.priceIn" :min="0" :precision="2" :step="0.5" :controls="false" size="small" />
            <span class="muted small">命中缓存</span>
            <el-input-number v-model="draft.priceCache" :min="0" :precision="2" :step="0.1" :controls="false" size="small" />
            <span class="muted small">输出</span>
            <el-input-number v-model="draft.priceOut" :min="0" :precision="2" :step="0.5" :controls="false" size="small" />
            <el-select v-model="draft.currency" size="small" style="width: 64px">
              <el-option value="¥" label="¥" />
              <el-option value="$" label="$" />
            </el-select>
          </div>
          <div class="muted small">每百万 tokens 的价格，可以不填；填了对话时显示大约花了多少钱</div>
        </el-form-item>
        <el-form-item>
          <el-button :loading="testing" @click="test">测试</el-button>
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
.models {
  margin: 4px 0 8px;
}
.empty {
  padding: 6px 0;
}
.model {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 4px 8px;
  border: 1px solid var(--el-border-color-lighter);
  border-radius: 6px;
  margin-bottom: 6px;
}
.model.def {
  border-color: var(--el-color-primary-light-5);
}
.model .el-radio {
  margin-right: 0;
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
.ml {
  margin-left: 8px;
}
.row {
  display: flex;
  gap: 8px;
  width: 100%;
}
.warn-line {
  color: var(--el-color-warning);
  line-height: 1.6;
  margin-top: 4px;
}
.prices {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
}
.prices .el-input-number {
  width: 76px;
}
.ok {
  color: var(--el-color-success);
}
.bad {
  color: var(--el-color-danger);
}
</style>
