<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { Refresh } from '@element-plus/icons-vue'
import { ferry, type AppRuntimeChoices, type AppRuntimeChoice } from '../ferry'
import { readCall, useReadRequests } from '../readRequests'
import { readConfig, writeConfig } from '../platform'
import { runtimeSwitchDraft } from '../editor/runtimeSwitch'
import { diffHunks } from '../editor/configDoc'
import type { DeploymentRequest } from '../editor/appSetup'
import LoadingBlock from './LoadingBlock.vue'

// prefer：打开时先选中的已有站点（网站目录），部署审阅里「改成更新这个站点的文件」用
const props=defineProps<{config:string;target:string;app:string;prefer?:string}>()
const emit=defineEmits<{close:[];saved:[deployment?:DeploymentRequest];versions:[]}>()
const visible=ref(true)
const loading=ref(true)
const saving=ref(false)
const error=ref('')
const data=ref<AppRuntimeChoices>()
const original=ref('')
const selected=ref('')
const port=ref('')
let disposed=false
let seq=0
const reads=useReadRequests()
onBeforeUnmount(()=>{disposed=true;seq++})
async function load() {
  const token=++seq
  reads.cancelAll()
  loading.value=true;error.value=''
  try {
    const [config,choices]=await reads.run('choices',signal=>Promise.all([readConfig(props.config),readCall<AppRuntimeChoices>(['app-runtimes',props.target,props.app,'-c',props.config],signal)]))
    if(disposed||token!==seq)return
    original.value=config;data.value=choices
    const preferred=props.prefer?choices.choices.find((choice)=>choice.site&&choice.path===props.prefer&&!choice.disabled):undefined
    selected.value=preferred?.id??choices.choices.find((choice)=>choice.current && !choice.disabled)?.id??''
    port.value=choices.port??''
  } catch(e) {if(!disposed&&token===seq)error.value=(e as Error).message}
  finally{if(!disposed&&token===seq)loading.value=false}
}
onMounted(load)
const choice=computed(()=>data.value?.choices.find((choice)=>choice.id===selected.value))
const draft=computed(()=>{
  if(!data.value||!choice.value)return {text:'',error:''}
  try{return {text:runtimeSwitchDraft(original.value,props.target,props.app,data.value.type,choice.value,port.value),error:''}}
  catch(e){return {text:'',error:(e as Error).message}}
})
const diff=computed(()=>draft.value.text?diffHunks(original.value.replace(/\r\n/g,'\n'),draft.value.text.replace(/\r\n/g,'\n')):[])
const canSave=computed(()=>!!data.value?.canSwitch&&!!choice.value&&!choice.value.disabled&&!draft.value.error&&!!draft.value.text&&draft.value.text!==original.value&&!loading.value&&!saving.value)
function label(item:AppRuntimeChoice){return item.version?.split('\n')[0]||item.path}
/** 名称下面的说明：已有站点的第一行（是哪个 nginx）已经当名称了 */
function detail(item:AppRuntimeChoice){return item.site?item.version?.split('\n').slice(1).join('\n')??'':item.version??''}
const hasSites=computed(()=>!!data.value?.choices.some((item)=>item.site))
const siteChosen=computed(()=>!!choice.value?.site)
async function save(deploy:boolean) {
  if(!canSave.value)return
  saving.value=true;error.value=''
  const content=draft.value.text
  try {
    const checked=await ferry(['validate','-c',props.config,'--from-env'],undefined,{FERRY_CONFIG_CONTENT:content}).done
    if(disposed)return
    if(!checked.ok)throw new Error(checked.error||'配置校验失败')
    if(await readConfig(props.config)!==original.value)throw new Error('配置文件已被其他操作修改，请重新检测后再保存')
    if(disposed)return
    await writeConfig(props.config,content)
    emit('saved',deploy?{apps:[props.app],targets:[props.target]}:undefined)
    visible.value=false
  }catch(e){error.value=(e as Error).message}
  finally{saving.value=false}
}
</script>

<template>
  <el-dialog v-model="visible" :title="`${app} · 切换运行环境（${target}）`" width="min(900px, 94vw)" top="5vh" append-to-body :close-on-click-modal="false" :show-close="!saving" :close-on-press-escape="!saving" @closed="emit('close')">
    <div class="switch-content">
      <div class="switch-heading"><span>选择服务器上已有的运行环境</span><el-button size="small" :icon="Refresh" :loading="loading" :disabled="saving" @click="load">重新检测</el-button></div>
      <LoadingBlock v-if="loading" text="正在检测已安装的运行环境和可用权限…" :rows="5" min-height="240px" />
      <template v-else-if="data">
        <el-alert v-if="!data.canSwitch" :title="data.reason" type="info" :closable="false" show-icon />
        <p v-if="data.current" class="muted current-runtime">当前：<span class="mono">{{ data.current }}</span></p>
        <p v-for="(note,index) in data.notes??[]" :key="index" class="muted small">{{ note }}</p>
        <el-radio-group v-if="data.choices.length" v-model="selected" class="runtime-choices" :disabled="!data.canSwitch || saving">
          <el-radio v-for="item in data.choices" :key="item.id" :value="item.id" :disabled="!!item.disabled" class="runtime-choice">
            <span class="choice-body"><span class="choice-heading"><strong>{{ label(item) }}</strong><el-tag v-if="item.current" size="small" type="success">当前</el-tag><el-tag size="small" type="info">{{ item.source }}</el-tag></span><span class="mono choice-path">{{ item.path }}</span><span v-if="item.unit" class="muted small">{{ item.unit }} · {{ item.state }}</span><span v-if="detail(item)" class="muted small choice-version">{{ detail(item) }}</span><span v-if="item.site?.owner" class="muted small">目录属主 {{ item.site.owner }}</span><span v-if="item.disabled" class="ev-warn small">{{ item.disabled }}</span></span>
          </el-radio>
        </el-radio-group>
        <el-empty v-else-if="data.canSwitch" description="没有检测到可用的运行环境" :image-size="60" />
        <p v-if="hasSites && !siteChosen" class="muted small">标着「已有站点」的是服务器上原来就有的 nginx / OpenResty（1Panel、宝塔、容器里的也算）里的站点：选它，部署时只把每一版的文件换进它的网站目录，不改它的配置。</p>
        <el-alert v-if="siteChosen && choice?.site" type="info" :closable="false" show-icon class="site-port">
          <template #title>部署时把每一版的文件换进 <span class="mono">{{ choice.path }}</span>，不改 {{ choice.site.server }}的配置，也不重启它。</template>
          <div class="small">第一次部署前，目录里原来的文件会备份到服务器上 Ferry 的应用目录：部署失败自动换回去，以后也能在「版本」里「回到接管前」。<template v-if="choice.site.ports?.length && !choice.site.shared">部署后检查 {{ choice.site.ports[0] }} 端口的首页能打开。</template><template v-else-if="choice.site.shared">这个端口上还有别的站点（按域名区分），部署后不检查首页；需要时在配置里写 static.health。</template></div>
        </el-alert>
        <el-form v-if="data.canSwitch && data.type==='static' && !siteChosen" label-width="100px" class="site-port"><el-form-item label="网站端口"><div class="full"><el-input v-model="port" placeholder="如 8081 或 127.0.0.1:8081" /><div class="muted small">不同 Nginx 实例不能同时占用相同地址和端口；原端口被其他站点共用时，请改用空闲端口。</div></div></el-form-item></el-form>
        <p v-if="data.canSwitch" class="muted small">只修改 {{ target }} 上 {{ app }} 的运行环境。保存配置后，重新部署才会生效；“保存并部署”会打开部署审阅，并使用当前本地产物，保留健康检查与失败回滚。</p>
        <el-collapse v-if="diff.length" class="config-diff"><el-collapse-item title="查看配置修改" name="diff"><pre class="mono"><template v-for="(h,index) in diff" :key="index"><span v-for="(line,lineIndex) in h.lines" :key="lineIndex" :class="line.t==='+'?'added':line.t==='-'?'removed':''">{{ line.t }} {{ line.text }}{{ '\n' }}</span></template></pre></el-collapse-item></el-collapse>
      </template>
      <el-alert v-if="error || draft.error" :title="error || draft.error" type="error" :closable="false" show-icon class="switch-error" />
    </div>
    <template #footer><el-button :disabled="saving" @click="visible=false">取消</el-button><el-button v-if="data && !data.canSwitch && ['binary','dotnet'].includes(data.type)" type="primary" @click="emit('versions')">查看已有版本</el-button><el-button v-if="data?.canSwitch" :disabled="!canSave" :loading="saving" @click="save(false)">仅保存配置</el-button><el-button v-if="data?.canSwitch" type="primary" :disabled="!canSave" :loading="saving" @click="save(true)">保存并部署</el-button></template>
  </el-dialog>
</template>

<style scoped>
.switch-content{min-height:200px;max-height:64dvh;overflow-y:auto;padding-right:6px}
.switch-heading,.choice-heading{display:flex;align-items:center;gap:10px;justify-content:space-between;margin-bottom:12px}
.choice-heading{justify-content:flex-start;flex-wrap:wrap;margin:0}
.runtime-choices{display:flex;flex-direction:column;align-items:stretch;width:100%;gap:8px}
.runtime-choice{height:auto!important;margin:0!important;padding:14px;border:1px solid var(--el-border-color);border-radius:8px;align-items:flex-start}
.runtime-choice.is-checked{border-color:var(--el-color-primary);background:var(--el-color-primary-light-9)}
.runtime-choice :deep(.el-radio__input){padding-top:6px}
.runtime-choice :deep(.el-radio__label){min-width:0;width:100%;white-space:normal}
.choice-body{display:flex;flex-direction:column;gap:5px;overflow-wrap:anywhere;line-height:1.6}
.choice-version{white-space:pre-wrap}.choice-path,.current-runtime{overflow-wrap:anywhere}
.site-port,.switch-error{margin-top:16px}.full{width:100%}
.config-diff pre{white-space:pre-wrap;overflow-wrap:anywhere;margin:0;max-height:220px;overflow:auto}
.added{background:var(--el-color-success-light-9)}.removed{background:var(--el-color-danger-light-9)}
</style>
