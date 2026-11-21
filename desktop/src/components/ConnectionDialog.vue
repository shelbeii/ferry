<script setup lang="ts">
// 新建 / 修改连接：填主机、端口、用户和登录方式。保存后它出现在项目列表里，选中后右边显示终端，文件点「文件」打开。
// 保存在数据目录的 connections.yaml 里；密码不写进文件：勾选「记住」时存系统凭据库，否则只在这次打开 Ferry 期间记住。
import { reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { call, clearSessionCredential, setSessionCredential, type Connection } from '../ferry'
import { pickMany } from '../platform'
import { CONN_PREFIX, loadConnections, selectProject } from '../state'

const props = defineProps<{ conn?: Connection }>()
const emit = defineEmits<{ close: [] }>()

const visible = ref(true)
const saving = ref(false)
const error = ref('')
const savedWithoutCredentials = ref(false)
const editing = !!props.conn
const savedName = ref(props.conn?.name ?? '')
const form = reactive({
  title: props.conn?.title ?? '',
  host: props.conn?.host ?? '',
  port: props.conn?.port ?? 22,
  user: props.conn?.user ?? 'root',
  auth: (props.conn?.auth ?? 'password') as 'key' | 'password',
  sudoAuth: props.conn?.sudoAuth ?? (editing ? '' : 'disabled'),
  sudoPassword: '',
  identityFile: props.conn?.identityFile ?? '',
  password: '',
  remember: true,
})

async function pickKey() {
  const [p] = await pickMany('选择私钥文件', false)
  if (p) form.identityFile = p
}

async function save(connect: boolean) {
  if (saving.value) return
  error.value = ''
  if (!form.host.trim()) {
    error.value = '请填写主机'
    return
  }
  if (!form.user.trim()) {
    error.value = '请填写用户名'
    return
  }
  if (!editing && form.auth === 'password' && !form.password) {
    error.value = '请填写密码'
    return
  }
  saving.value = true
  savedWithoutCredentials.value = false
  const login = { auth: form.auth, password: form.password, remember: form.remember }
  let pending = '连接设置'
  try {
    const value = {
      name: savedName.value,
      title: form.title.trim(),
      host: form.host.trim(),
      port: Number(form.port) || 22,
      user: form.user.trim(),
      auth: form.auth,
      sudoAuth: form.user.trim() === 'root' ? '' : form.sudoAuth,
      identityFile: form.auth === 'key' ? form.identityFile.trim() : '',
    }
    const r = await call<{ name: string; path: string; passwordEnv: string }>(['connection', 'save'], { FERRY_CONNECTION_VALUE: JSON.stringify(value) })
    savedName.value = r.name
    savedWithoutCredentials.value = true
    if (login.auth === 'password' && login.password) {
      pending = '登录密码'
      if (login.remember) {
        // 值只经环境变量传给 ferry，不出现在命令行参数里
        await call(['secret', 'set', r.name, '-c', r.path], { FERRY_SECRET_VALUE: login.password })
        clearSessionCredential(r.passwordEnv)
      } else {
        setSessionCredential(r.passwordEnv, login.password)
      }
    }
    if (value.sudoAuth === 'password' && form.sudoPassword) {
      pending = 'sudo 密码'
      const saved = await call<{ env: string; passwordless?: boolean }>(['secret', 'set', '--sudo', '--verify', r.name, '-c', r.path], { FERRY_SECRET_VALUE: form.sudoPassword })
      clearSessionCredential(saved.env)
      if (saved.passwordless) ElMessage.info('服务器未要求 sudo 密码')
    }
    pending = '连接列表'
    await loadConnections()
    // 保存后在项目列表里选中它：右边显示这个连接，并打开终端
    if (connect || !editing) await selectProject(CONN_PREFIX + r.name)
    if (!connect) ElMessage.success('已保存')
    savedWithoutCredentials.value = false
    visible.value = false
  } catch (e) {
    const message = (e as Error).message
    error.value = savedWithoutCredentials.value ? `连接设置已保存，${pending}未能更新。 ${message}` : message
    if (savedWithoutCredentials.value) await loadConnections().catch(() => {})
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <el-dialog v-model="visible" :title="editing ? '修改连接' : '新建连接'" width="520px" append-to-body :before-close="(done: () => void) => { if (!saving) done() }" @closed="emit('close')">
    <el-form label-width="84px" @submit.prevent>
      <el-form-item label="主机" required>
        <div class="row">
          <el-input v-model="form.host" placeholder="IP 或域名" autofocus class="grow" />
          <span class="muted">端口</span>
          <el-input-number v-model="form.port" :min="1" :max="65535" :controls="false" class="port" />
        </div>
      </el-form-item>
      <el-form-item label="用户名" required>
        <el-input v-model="form.user" placeholder="root" />
      </el-form-item>
      <el-form-item label="登录方式">
        <el-radio-group v-model="form.auth">
          <el-radio-button value="password">密码</el-radio-button>
          <el-radio-button value="key">密钥</el-radio-button>
        </el-radio-group>
      </el-form-item>
      <template v-if="form.auth === 'password'">
        <el-form-item label="密码" :required="!editing">
          <el-input v-model="form.password" type="password" show-password :placeholder="editing ? '不改就留空' : ''" @keyup.enter="save(true)" />
        </el-form-item>
        <el-form-item label=" ">
          <el-checkbox v-model="form.remember">记住到系统凭据库（Windows 凭据管理器 / macOS 钥匙串）</el-checkbox>
          <div class="muted small">不勾选时只在这次打开 Ferry 期间记住。密码不会写进任何文件。</div>
        </el-form-item>
      </template>
      <el-form-item v-else label="私钥">
        <div class="row">
          <el-input v-model="form.identityFile" placeholder="留空 = 用本机 .ssh 里的默认私钥" class="grow" />
          <el-button @click="pickKey">选择…</el-button>
        </div>
      </el-form-item>
      <el-form-item label="名称">
        <el-input v-model="form.title" :placeholder="`${form.user || 'root'}@${form.host || '主机'}`" />
      </el-form-item>
      <el-form-item v-if="form.user.trim() && form.user.trim() !== 'root'" label="sudo 认证">
        <div>
          <el-radio-group v-model="form.sudoAuth">
            <el-radio-button v-if="form.sudoAuth === ''" value="">沿用默认</el-radio-button>
            <el-radio-button value="disabled">禁用</el-radio-button>
            <el-radio-button value="nopasswd">免密</el-radio-button>
            <el-radio-button value="password">使用密码</el-radio-button>
          </el-radio-group>
        </div>
      </el-form-item>
      <el-form-item v-if="form.user.trim() && form.user.trim() !== 'root' && form.sudoAuth === 'password'" label="sudo 密码">
        <div class="grow">
          <el-input v-model="form.sudoPassword" type="password" show-password autocomplete="new-password" placeholder="留空可在首次提权时输入" />
          <div class="muted small">在这里填写的 sudo 密码保存到系统凭据库。</div>
        </div>
      </el-form-item>
    </el-form>
    <div class="muted small">第一次连接时会请你核对服务器的主机公钥。</div>
    <el-alert v-if="error" type="error" :title="error" :closable="false" show-icon class="mt" />
    <template #footer>
      <el-button :disabled="saving" @click="visible = false">{{ savedWithoutCredentials ? '关闭' : '取消' }}</el-button>
      <el-button :loading="saving" @click="save(false)">只保存</el-button>
      <el-button type="primary" :loading="saving" @click="save(true)">保存并连接</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.row {
  display: flex;
  align-items: center;
  gap: 8px;
  width: 100%;
}
.grow {
  flex: 1;
}
.port {
  width: 90px;
}
.small {
  font-size: 12px;
  line-height: 1.6;
}
.mt {
  margin-top: 10px;
}
</style>
