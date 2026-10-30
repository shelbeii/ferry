<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { Hide, View } from '@element-plus/icons-vue'
import { ferry, type Running, type SecretEntry } from '../ferry'
import LoadingBlock from './LoadingBlock.vue'

const props = defineProps<{ entry: SecretEntry }>()
const emit = defineEmits<{ close: [] }>()
const visible = ref(true)
const value = ref('')
const revealed = ref(false)
const loading = ref(false)
const loaded = ref(false)
const error = ref('')
const label = computed(() => {
  if (['password', 'passphrase', 'software'].includes(props.entry.kind)) return '密码'
  if (props.entry.kind === 'ai') return 'API Key'
  if (props.entry.account.startsWith('notify:url:')) return '通知地址'
  if (props.entry.account.startsWith('notify:sign:')) return '加签密钥'
  return '凭据内容'
})

let request: Running<{ value: string }> | undefined
let serial = 0
function clear() {
  serial++ // 关闭后仍到达的响应不能重新填入密码。
  request?.kill()
  request = undefined
  value.value = ''
  revealed.value = false
  loaded.value = false
  loading.value = false
}

async function load() {
  clear()
  error.value = ''
  loading.value = true
  const seq = serial
  // 单独调用 sidecar：不经过 startTask，不把凭据写入任务、对话或设置。
  const proc = ferry<{ value: string }>(['secret', 'show', '--account', props.entry.account])
  request = proc
  try {
    const result = await proc.done
    if (seq !== serial || !visible.value) return
    if (!result.ok || typeof result.data?.value !== 'string') {
      error.value = result.error ?? '读取凭据失败'
      return
    }
    value.value = result.data.value
    loaded.value = true
  } catch {
    if (seq === serial && visible.value) error.value = '读取凭据失败，请重试'
  } finally {
    if (seq === serial) {
      request = undefined
      loading.value = false
    }
  }
}

watch(visible, (open) => { if (!open) clear() }, { flush: 'sync' })
onMounted(load)
onBeforeUnmount(clear)
</script>

<template>
  <el-dialog v-model="visible" title="查看凭据" width="560px" append-to-body destroy-on-close @close="emit('close')">
    <div class="account">
      <span class="muted small">账户</span>
      <span class="mono">{{ entry.label }}</span>
    </div>
    <LoadingBlock v-if="loading" text="正在读取系统凭据库…" :rows="2" min-height="70px" />
    <el-alert v-else-if="error" type="error" :title="error" :closable="false" show-icon />
    <template v-else-if="loaded">
      <label class="value-label" for="saved-credential-value">{{ label }}</label>
      <el-input
        id="saved-credential-value"
        :model-value="value"
        :type="revealed ? 'text' : 'password'"
        readonly
        autocomplete="off"
        :spellcheck="false"
        class="mono"
      >
        <template #append>
          <el-button :icon="revealed ? Hide : View" :aria-label="`${revealed ? '隐藏' : '显示'}${label}`" :aria-pressed="revealed" @click="revealed = !revealed">
            {{ revealed ? '隐藏' : '显示' }}
          </el-button>
        </template>
      </el-input>
      <p class="muted small">这里是本机保存的内容；如果在服务器或服务商那里修改过，这里不会自动更新。</p>
    </template>
    <template #footer>
      <el-button v-if="error" :loading="loading" @click="load">重新读取</el-button>
      <el-button type="primary" @click="visible = false">关闭</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.account {
  display: flex;
  flex-direction: column;
  gap: 6px;
  margin-bottom: 20px;
  overflow-wrap: anywhere;
}
.value-label {
  display: block;
  margin-bottom: 8px;
}
p {
  margin: 14px 0 0;
  line-height: 1.7;
}
</style>
