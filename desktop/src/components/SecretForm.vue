<script setup lang="ts">
// AI 助手要你填的密码、账号（ask_secret）：填的值只交给本机的 Ferry，AI 只拿到代号，看不到值。
import { computed, reactive, ref } from 'vue'
import { Lock } from '@element-plus/icons-vue'

export interface SecretField {
  name: string
  label: string
  kind?: 'password' | 'text'
  confirm?: boolean
}
export interface SecretValue {
  name: string
  value: string
  remember?: boolean
  label?: string
}

const props = defineProps<{ fields: SecretField[]; disabled?: boolean; submitText?: string; cancelText?: string; noRemember?: boolean }>()
const emit = defineEmits<{ submit: [values: SecretValue[]]; cancel: [] }>()

const values = reactive<Record<string, string>>({})
const again = reactive<Record<string, string>>({})
const remember = ref(false)
const tried = ref(false)

function problem(f: SecretField): string {
  if (!values[f.name]) return '还没填'
  if (f.confirm && again[f.name] !== values[f.name]) return '两次输入的不一样'
  return ''
}
const ok = computed(() => props.fields.every((f) => !problem(f)))

function submit() {
  tried.value = true
  if (!ok.value || props.disabled) return
  const out = props.fields.map((f) => ({ name: f.name, value: values[f.name], remember: remember.value, label: f.label }))
  // 交出去后清掉，界面上不留
  for (const f of props.fields) {
    values[f.name] = ''
    again[f.name] = ''
  }
  emit('submit', out)
}
</script>

<template>
  <form class="secret-form" autocomplete="off" @submit.prevent="submit">
    <div class="muted small tip"><el-icon><Lock /></el-icon>填的内容只交给这台电脑上的 Ferry，执行时才用上；AI 只拿到代号，看不到内容</div>
    <div v-for="f in fields" :key="f.name" class="row">
      <label class="lbl small">{{ f.label }}</label>
      <el-input
        v-model="values[f.name]"
        size="small"
        :type="f.kind === 'text' ? 'text' : 'password'"
        :show-password="f.kind !== 'text'"
        :disabled="disabled"
        :aria-label="f.label"
        autocomplete="off"
      />
      <el-input
        v-if="f.confirm"
        v-model="again[f.name]"
        size="small"
        class="again"
        :type="f.kind === 'text' ? 'text' : 'password'"
        :show-password="f.kind !== 'text'"
        :disabled="disabled"
        placeholder="再输一遍"
        :aria-label="`${f.label}（再输一遍）`"
        autocomplete="off"
      />
      <div v-if="tried && problem(f)" class="err small">{{ problem(f) }}</div>
    </div>
    <el-checkbox v-if="!noRemember" v-model="remember" size="small" :disabled="disabled">记住（存进系统凭据库，在「设置 → 凭据」里能查看、删除；不选的话 Ferry 退出就忘掉）</el-checkbox>
    <div class="btns">
      <el-button size="small" type="primary" native-type="submit" :disabled="disabled">{{ submitText ?? '填好了' }}</el-button>
      <el-button size="small" :disabled="disabled" @click="emit('cancel')">{{ cancelText ?? '不填' }}</el-button>
    </div>
  </form>
</template>

<style scoped>
.secret-form {
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.tip {
  display: flex;
  align-items: flex-start;
  gap: 4px;
  line-height: 1.5;
}
.tip .el-icon {
  margin-top: 2px;
  flex: none;
}
.row {
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.lbl {
  font-weight: 500;
}
.err {
  color: var(--el-color-danger);
}
.btns {
  display: flex;
  gap: 8px;
}
.secret-form :deep(.el-checkbox) {
  white-space: normal;
  height: auto;
  line-height: 1.4;
}
</style>
