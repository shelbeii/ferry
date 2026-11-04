<script setup lang="ts">
// DNS 验证密钥与设置一起提交，成功后才清理草稿。
// 部署时带到服务器上存一份（只有 root 能读），服务器每天续期证书时用。
import { onMounted, reactive, ref } from 'vue'
import { call } from '../ferry'
import { dnsSecretChanges } from '../settingsSecrets'

type Provider = 'aliyun' | 'dnspod' | 'cloudflare'
const providers: { id: Provider; name: string; idLabel?: string; secretLabel: string; tip: string }[] = [
  { id: 'aliyun', name: '阿里云 DNS', idLabel: 'AccessKey ID', secretLabel: 'AccessKey Secret', tip: '建一个 RAM 用户，只给 AliyunDNSFullAccess 权限，用它的 AccessKey' },
  { id: 'dnspod', name: '腾讯云 DNSPod', idLabel: 'SecretId', secretLabel: 'SecretKey', tip: '建一个子用户，只给 QcloudDNSPodFullAccess 权限，用它的 API 密钥' },
  { id: 'cloudflare', name: 'Cloudflare', secretLabel: 'API Token', tip: 'My Profile → API Tokens → Create Token，权限选 Zone · DNS · Edit' },
]

const stored = ref<Record<string, boolean>>({})
const draft = reactive<Record<string, { id: string; secret: string }>>({
  aliyun: { id: '', secret: '' },
  dnspod: { id: '', secret: '' },
  cloudflare: { id: '', secret: '' },
})
const removed = reactive(new Set<string>())

onMounted(async () => {
  try {
    stored.value = await call<Record<string, boolean>>(['dns-key', 'list'])
  } catch {
    stored.value = {}
  }
})

const has = (p: Provider) => !removed.has(p) && !!stored.value[p]

function getSecretChanges() { return dnsSecretChanges(draft, removed) }
function acceptSecretChanges() {
  for (const p of providers) {
    const d = draft[p.id]
    if (d.secret.trim()) {
      d.id = d.secret = ''
      removed.delete(p.id)
      stored.value[p.id] = true
    } else if (removed.has(p.id)) {
      stored.value[p.id] = false
      removed.delete(p.id)
    }
  }
}
defineExpose({ getSecretChanges, acceptSecretChanges })
</script>

<template>
  <div class="dns">
    <div v-for="p in providers" :key="p.id" class="provider">
      <div class="head">
        <span class="name">{{ p.name }}</span>
        <el-tag v-if="has(p.id)" size="small" type="success" effect="plain" disable-transitions>已保存</el-tag>
        <el-button v-if="has(p.id)" text size="small" type="danger" @click="removed.add(p.id)">删除</el-button>
        <span class="muted small tip">{{ p.tip }}</span>
      </div>
      <div class="row">
        <el-input v-if="p.idLabel" v-model="draft[p.id].id" class="mono" :placeholder="p.idLabel" />
        <el-input v-model="draft[p.id].secret" type="password" show-password class="mono" :placeholder="has(p.id) ? p.secretLabel + '（已保存，填写则替换）' : p.secretLabel" />
      </div>
    </div>
  </div>
</template>

<style scoped>
.provider {
  margin-bottom: 10px;
}
.head {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 4px;
}
.name {
  font-weight: 600;
}
.tip {
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.row {
  display: flex;
  gap: 8px;
}
.small {
  font-size: 12px;
}
</style>
