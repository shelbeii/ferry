<script setup lang="ts">
import LoadingBlock from './LoadingBlock.vue'
import { computed, onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { call, type HostKey } from '../ferry'
import { readCall, ReadCanceled, useReadRequests } from '../readRequests'
import { useInteractionScope } from '../interactionScope'

// retrying：记录后会自动接着执行刚才的操作
const props = defineProps<{ config: string; target: string; retrying?: boolean }>()
const emit = defineEmits<{ close: []; trusted: [] }>()

const visible = ref(true)
const captureContext = useInteractionScope(() => visible.value)
const reads = useReadRequests()
const loading = ref(true)
const saving = ref(false)
const key = ref<HostKey>()
const error = ref('')
const publicKeyFile = computed(() => {
  switch (key.value?.type) {
    case 'ssh-ed25519': return '/etc/ssh/ssh_host_ed25519_key.pub'
    case 'ecdsa-sha2-nistp256':
    case 'ecdsa-sha2-nistp384':
    case 'ecdsa-sha2-nistp521': return '/etc/ssh/ssh_host_ecdsa_key.pub'
    case 'ssh-rsa':
    case 'rsa-sha2-256':
    case 'rsa-sha2-512': return '/etc/ssh/ssh_host_rsa_key.pub'
    default: return ''
  }
})

onMounted(async () => {
  try {
    key.value = await reads.run('hostkey', s => readCall<HostKey>(['hostkey', props.target, '-c', props.config], s))
  } catch (e) {
    if (e instanceof ReadCanceled) return
    error.value = (e as Error).message
  } finally {
    loading.value = false
  }
})

async function trust() {
  if (!key.value || saving.value) return
  const current = captureContext()
  saving.value = true
  try {
    // --expect：只有服务器此刻的指纹与用户看到并核对过的一致才记录
    await call(['trust', props.target, '-c', props.config, '--expect', key.value.fingerprint])
    if (!current()) return
    ElMessage.success(props.retrying ? '已记录主机公钥，正在接着执行刚才的操作' : '已记录主机公钥，可以重新执行刚才的操作')
    emit('trusted')
    close()
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    saving.value = false
  }
}

function close() {
  visible.value = false
  emit('close')
}
</script>

<template>
  <el-dialog v-model="visible" :title="`核对主机公钥：${target}`" width="620px" append-to-body :before-close="(done: () => void) => { if (!saving) done() }" @closed="emit('close')">
    <div class="body">
      <LoadingBlock v-if="loading" :text="`正在连接 ${target}，读取主机公钥…`" :rows="3" min-height="140px" />
      <el-alert v-if="error" type="error" :title="error" :closable="false" show-icon />
      <template v-if="key">
        <p>
          第一次连接 <strong>{{ key.host }}:{{ key.port }}</strong>。为防止连到冒充的服务器，请通过可信渠道（服务器控制台、云厂商面板）核对公钥指纹：
        </p>
        <div class="fp mono">{{ key.type }}<br />{{ key.fingerprint }}</div>
        <p v-if="publicKeyFile" class="muted">在可信的服务器控制台执行 <span class="mono">ssh-keygen -l -E sha256 -f {{ publicKeyFile }}</span>。使用自定义主机密钥时，核对 SSH 服务配置中的对应公钥。</p>
        <p v-else class="muted">从 SSH 服务配置确认正在使用的主机公钥，再核对其 SHA-256 指纹。</p>
        <el-alert v-if="key.state === 'known'" type="success" title="这个公钥已经记录过了" :closable="false" show-icon />
        <el-alert
          v-if="key.state === 'mismatch'"
          type="error"
          title="与之前记录的公钥不一致！可能遭到中间人攻击，或服务器重装过。确认服务器确实重装过之后，才从 known_hosts 删除旧记录。"
          :closable="false"
          show-icon
        />
      </template>
    </div>
    <template #footer>
      <el-button :disabled="saving" @click="close">取消</el-button>
      <el-button type="primary" :disabled="!key || key.state !== 'unknown'" :loading="saving" @click="trust">指纹一致，记录</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.body {
  min-height: 120px;
}
.fp {
  background: var(--el-fill-color-light);
  border-radius: 6px;
  padding: 10px 12px;
  font-size: 14px;
  user-select: all;
}
</style>
