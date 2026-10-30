<script setup lang="ts">
import { ref } from 'vue'
import { ElMessage } from 'element-plus'
import { call } from '../ferry'

// 软件模板编辑器：检查、保存到自己的模板目录（ID 和内置的一样时覆盖内置的）
const props = defineProps<{ content: string; title: string }>()
const emit = defineEmits<{ close: []; saved: [] }>()

const text = ref(props.content)
const error = ref('')
const okMsg = ref('')
const busy = ref(false)
const helpOpen = ref<string[]>([])

async function run(action: 'check' | 'save') {
  busy.value = true
  error.value = ''
  okMsg.value = ''
  try {
    const t = await call<{ id: string; name: string }>(['software', 'template', action], { FERRY_SOFTWARE_TEMPLATE: text.value })
    if (action === 'save') {
      ElMessage.success(`模板 ${t.id}（${t.name}）已保存`)
      emit('saved')
      emit('close')
    } else {
      okMsg.value = `没有问题：${t.id}（${t.name}）`
    }
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <el-dialog :model-value="true" :title="title" width="900px" top="4vh" class="fit-dialog" :close-on-click-modal="false" @close="emit('close')">
    <el-input v-model="text" type="textarea" :rows="24" class="mono editor" spellcheck="false" />
    <el-alert v-if="error" type="error" :title="error" :closable="false" show-icon class="msg" />
    <el-alert v-if="okMsg" type="success" :title="okMsg" :closable="false" show-icon class="msg" />
    <el-collapse v-model="helpOpen" class="help">
      <el-collapse-item title="模板怎么写" name="1">
        <div class="small help-body">
          <p>
            <b>id / name / category / desc</b>：模板 ID（小写字母、数字、横线）、显示名、分类（数据库、缓存、中间件、存储、Web 服务器、工具、其他、自定义）、说明。
            ID 和内置模板一样时，用这里的代替内置的。
          </p>
          <p><b>params</b>：安装时填的参数，type 为 text、port、password、bool、memory、select、number；port 可以写 offsets（还要占用 +N 的端口）。key 为 password 的密码会存进本机的凭据库。</p>
          <p>
            <b>variants</b>：几种装法，安装时按服务器选第一种合适的。os 写 /etc/os-release 的 ID 或 ID_LIKE（rhel、centos、fedora、debian、ubuntu……），arch 写
            amd64、arm64；不写就是都可以。
          </p>
          <p>
            程序从哪来：<b>package</b>（上传安装包：url、file 官方文件名、github 或 probe 查最新版本、fallback、format、strip、into、check）或 <b>system</b>（用服务器的软件源：dnf、apt
            的包名，disable 装包后停掉的系统自带服务，require 必须有的程序）。
          </p>
          <p>
            <b>files</b>（写文件，at: ready 为就绪后改写）、<b>props</b>（改 key=value 配置）、<b>steps</b>（在服务器上以 root 执行：at 为 prepare、init、ready；run 或
            http；error 失败时的说明；soft 失败只提示）、<b>service</b>（exec 启动命令、workdir、env 放密码、ready.port 等待的端口）、<b>logs</b>、<b>notes</b>。文件只能写在安装目录下。
          </p>
          <p>
            变量：<span class="mono">${name} ${dir} ${user} ${version} ${java} ${package} ${host}</span> 和参数；<span class="mono">${参数?是|否}</span>、<span class="mono">${参数+1000}</span>、
            <span class="mono">${random:24}</span>、<span class="mono">${base64random:48}</span>；<span class="mono">$${X}</span> 原样留下 <span class="mono">${X}</span>（给 systemd、shell 用）。
            files、steps 可以写 if: 参数（或 !参数）。
          </p>
          <p>可以先在模板列表里把内置的复制成自己的，照着改。</p>
        </div>
      </el-collapse-item>
    </el-collapse>
    <template #footer>
      <el-button @click="emit('close')">取消</el-button>
      <el-button :loading="busy" @click="run('check')">检查</el-button>
      <el-button type="primary" :loading="busy" @click="run('save')">保存</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.editor :deep(textarea) {
  font-size: 12px;
  line-height: 1.5;
}
.msg {
  margin-top: 8px;
}
.help {
  margin-top: 8px;
}
.small {
  font-size: 12px;
}
.help-body p {
  margin: 4px 0;
  line-height: 1.7;
}
</style>
