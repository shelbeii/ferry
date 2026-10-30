<script setup lang="ts">
import { computed, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { call, sessionEnv, type ErrorDetail } from '../ferry'

const props = defineProps<{ detail: ErrorDetail; config: string }>()
const emit = defineEmits<{ close: []; done: [] }>()

const visible = ref(true)
const value = ref('')
const remember = ref(false)
const saving = ref(false)
const error = ref('')

const isPassphrase = computed(() => props.detail.kind.startsWith('passphrase'))
const rejected = computed(() => props.detail.kind.endsWith('-rejected'))
const title = computed(() => (isPassphrase.value ? '私钥密码' : '服务器登录密码'))

async function submit() {
  if (!value.value) return
  saving.value = true
  error.value = ''
  try {
    if (remember.value) {
      // 存进系统凭据库：值只经环境变量传给 ferry，不出现在命令行参数里
      const args = isPassphrase.value
        ? ['secret', 'set', '--passphrase', props.detail.key ?? '', '-c', props.config]
        : ['secret', 'set', props.detail.target ?? '', '-c', props.config]
      await call(args, { FERRY_SECRET_VALUE: value.value })
      delete sessionEnv[props.detail.env ?? '']
      ElMessage.success('已保存到系统凭据库')
    } else if (props.detail.env) {
      // 只在本次运行期间记住
      sessionEnv[props.detail.env] = value.value
    }
    visible.value = false
    emit('done')
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <el-dialog v-model="visible" :title="title" width="520px" @closed="emit('close')">
    <el-alert
      v-if="rejected"
      type="error"
      :closable="false"
      show-icon
      :title="isPassphrase ? '私钥密码不正确，请重新输入' : '服务器拒绝了这个密码，请重新输入'"
      class="mb"
    />
    <p>{{ detail.prompt }}</p>
    <el-input v-model="value" type="password" show-password autofocus placeholder="输入后回车" @keyup.enter="submit" />
    <el-checkbox v-model="remember" class="mt">记住到系统凭据库（Windows 凭据管理器 / macOS 钥匙串），以后不再询问</el-checkbox>
    <p class="muted small">
      不勾选时只在本次打开 Ferry 期间记住。密码不会写进 ferry.yaml。
    </p>
    <el-alert v-if="error" type="error" :title="error" :closable="false" show-icon />
    <template #footer>
      <el-button @click="visible = false">取消</el-button>
      <el-button type="primary" :disabled="!value" :loading="saving" @click="submit">确定并重试</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.mb {
  margin-bottom: 10px;
}
.mt {
  margin-top: 10px;
}
.small {
  font-size: 12px;
  line-height: 1.6;
}
</style>
