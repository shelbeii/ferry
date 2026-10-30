<script setup lang="ts">
import { ref } from 'vue'
import { ElMessage } from 'element-plus'
import { passwordEnv, type InspectTarget, type KeyGenResult } from '../ferry'
import { startTask } from '../state'

// onFinished（@finished）写成属性而不是事件：点「开始」后对话框就关了、组件已卸载，那时发出的事件 Vue 会丢掉，页面就不刷新
const props = defineProps<{ config: string; project: string; target: InspectTarget; onFinished?: () => void }>()
const emit = defineEmits<{ close: [] }>()

const visible = ref(true)
const password = ref('')
const keyPath = ref('')
// 私钥记到哪里：本机设置（只对这台电脑）/ ferry.yaml（所有电脑共用）/ 不记
const to = ref<'local' | 'yaml' | ''>('local')

function start() {
  const args = ['keygen', props.target.name, '-c', props.config]
  if (keyPath.value.trim()) args.push('--key', keyPath.value.trim())
  if (to.value === 'local') args.push('--local')
  else if (to.value === 'yaml') args.push('--write')
  // 密码只通过环境变量交给这一次 ferry，不保存
  const env: Record<string, string> = password.value ? { [passwordEnv(props.target.name)]: password.value } : {}
  startTask(
    `配置免密登录：${props.target.name}`,
    args,
    (t) => {
      if (t.status === 'ok') {
        const r = t.data as KeyGenResult
        ElMessage.success(
          r.localUpdated
            ? '免密登录已配置，已记到这台电脑的本机设置'
            : r.configUpdated
              ? '免密登录已配置，并已写入 ferry.yaml'
              : `免密登录已配置：私钥在 ${r.keyPath}`,
        )
        props.onFinished?.()
      }
    },
    env,
  )
  password.value = ''
  visible.value = false
}
</script>

<template>
  <el-dialog v-model="visible" :title="`配置免密登录：${target.name}`" width="600px" @closed="emit('close')">
    <ol class="steps">
      <li>在这台电脑上生成一对 ed25519 密钥（已有则复用），默认保存在 Ferry 数据目录的 <span class="mono">keys/{{ project }}-{{ target.name }}_ed25519</span>；</li>
      <li>用下面的密码登录 <span class="mono">{{ target.user }}@{{ target.host }}:{{ target.port }}</span> 一次，把公钥加入该账号的 <span class="mono">~/.ssh/authorized_keys</span>（已有不重复添加，并修正权限）；</li>
      <li>只用新密钥重新登录，确认免密登录可用；</li>
      <li>把私钥记到下面选的地方。</li>
    </ol>
    <el-form label-width="90px">
      <el-form-item label="登录密码">
        <el-input v-model="password" type="password" show-password placeholder="留空则使用已保存到系统凭据库的密码" />
        <div class="muted small">只用于这一次安装公钥，不会保存。</div>
      </el-form-item>
      <el-form-item label="私钥位置">
        <el-input v-model="keyPath" placeholder="留空用默认位置" />
      </el-form-item>
      <el-form-item label="记到">
        <el-radio-group v-model="to" class="to">
          <el-radio value="local">本机设置：只对这台电脑（推荐，别的电脑用同一份 ferry.yaml 不受影响）</el-radio>
          <el-radio value="yaml">ferry.yaml：所有电脑共用（只有这台电脑用这份配置时）</el-radio>
          <el-radio value="">不记，我自己写</el-radio>
        </el-radio-group>
      </el-form-item>
    </el-form>
    <template #footer>
      <el-button @click="visible = false">取消</el-button>
      <el-button type="primary" @click="start">开始</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.steps {
  margin: 0 0 14px;
  padding-left: 20px;
  line-height: 1.8;
  overflow-wrap: anywhere;
}
.small {
  font-size: 12px;
}
.to {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  width: 100%;
  min-width: 0;
}
.to :deep(.el-radio) { height: auto; max-width: 100%; align-items: flex-start; margin: 4px 0; white-space: normal; }
.to :deep(.el-radio__label) { white-space: normal; line-height: 1.6; }
.to :deep(.el-radio__input) { margin-top: 3px; }
</style>
