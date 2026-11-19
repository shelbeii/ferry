<script setup lang="ts">
import { computed, defineAsyncComponent, onMounted, ref } from 'vue'
import { ArrowLeft, CoffeeCup, Cpu, FolderOpened } from '@element-plus/icons-vue'
import type { Inspect } from '../ferry'
import { pickFile } from '../platform'
import { readCall, ReadCanceled, useReadRequests } from '../readRequests'

const RemoteDirectoryPicker = defineAsyncComponent(() => import('./RemoteDirectoryPicker.vue'))
const props = defineProps<{ config: string; target: string }>()
const emit = defineEmits<{ close: []; install: [name: string, version: string, args: string[]] }>()

const templates = [
  {
    id: 'jdk',
    name: 'JDK',
    description: '供 Java 应用和服务使用，支持按大版本或精确版本预装。',
    icon: CoffeeCup,
    versionExample: '21.0.5+11',
    directoryExample: '/opt/jdks',
    channels: [],
  },
  {
    id: 'node',
    name: 'Node.js',
    description: '安装官方 Linux 发行包，包含 npm。',
    icon: Cpu,
    versionExample: '24.0.0',
    directoryExample: '/opt/nodejs',
    channels: ['24', '26', '22'],
  },
  {
    id: 'python',
    name: 'Python',
    description: '安装 CPython，包含 pip 和 venv。',
    icon: Cpu,
    versionExample: '3.14.0',
    directoryExample: '/opt/python',
    channels: ['3.14', '3.15', '3.13', '3.12'],
  },
  {
    id: 'dotnet',
    name: '.NET',
    description: '安装 ASP.NET Core Runtime、.NET Runtime 或 SDK。',
    icon: Cpu,
    versionExample: '10.0.15',
    directoryExample: '/opt/dotnet',
    channels: ['10.0', '11.0'],
  },
]
const selected = ref<(typeof templates)[number]>()
const versionMode = ref<'major' | 'version'>('major')
const major = ref<number>()
const channel = ref('')
const component = ref('aspnetcore')
const exactVersion = ref('')
const localPackage = ref('')
const locationMode = ref<'shared' | 'custom'>('shared')
const directory = ref('')
const browsing = ref(false)
const sharedDirectory = ref('')
const sharedDirectoryError = ref('')
const reads = useReadRequests()

onMounted(async () => {
  const { config, target } = props
  try {
    const info = await reads.run('inspect', (signal) => readCall<Inspect>(['inspect', '-c', config], signal))
    sharedDirectory.value = info.targets.find((row) => row.name === target)?.runtimeDir ?? ''
    if (!sharedDirectory.value) sharedDirectoryError.value = '无法从服务器配置中读取公共目录'
  } catch (error) {
    if (!(error instanceof ReadCanceled)) sharedDirectoryError.value = '无法读取公共目录，请检查服务器配置'
  }
})

function normalizeDirectory(value: string): string {
  const trimmed = value.trim()
  return trimmed.length > 1 ? trimmed.replace(/\/+$/, '') || '/' : trimmed
}

function directoryError(value: string): string {
  return /^\/[A-Za-z0-9][A-Za-z0-9._-]*(\/[A-Za-z0-9][A-Za-z0-9._-]*)*$/.test(normalizeDirectory(value))
    ? '' : `请输入服务器上的绝对路径，例如 ${selected.value?.directoryExample ?? '/opt'}`
}

const installDir = computed(() => normalizeDirectory(directory.value))
const canInstall = computed(() => !!version.value && (!localPackage.value || versionMode.value === 'version') &&
  (locationMode.value === 'shared' || !directoryError(installDir.value)))
const version = computed(() => {
  if (versionMode.value === 'major') {
    if (selected.value?.id !== 'jdk') return channel.value
    return major.value !== undefined && Number.isInteger(major.value) && major.value >= 8 && major.value <= 99 ? String(major.value) : ''
  }
  const value = exactVersion.value.trim()
  const pattern = selected.value?.id === 'jdk'
    ? /^\d+(\.\d+)*\+\d+(-[A-Za-z0-9.]+)?$/
    : /^(0|[1-9]\d{0,4})(\.(0|[1-9]\d{0,4})){2}$/
  return pattern.test(value) ? value : ''
})

function selectTemplate(template: (typeof templates)[number]) {
  selected.value = template
  versionMode.value = 'major'
  major.value = 21
  channel.value = template.channels[0] ?? ''
  component.value = 'aspnetcore'
  exactVersion.value = ''
  localPackage.value = ''
  locationMode.value = 'shared'
  directory.value = ''
}

function submit() {
  const template = selected.value
  if (!template || !canInstall.value) return
  const jdk = template.id === 'jdk'
  const args = jdk ? ['software', 'jdk', 'install', props.target] : ['software', 'runtime', 'install', props.target, template.id]
  args.push(versionMode.value === 'version' ? '--version' : jdk ? '--java' : '--channel', version.value)
  if (locationMode.value === 'custom') args.push(jdk ? '--java-dir' : '--dir', installDir.value)
  if (template.id === 'dotnet') args.push('--component', component.value)
  if (localPackage.value) args.push('--package', localPackage.value)
  emit('install', template.name, version.value, args)
}

async function selectPackage() {
  const template = selected.value
  const file = await pickFile('选择目标服务器架构的 Linux .tar.gz 安装包', '运行环境安装包', ['gz'])
  if (file && selected.value === template) {
    versionMode.value = 'version'
    localPackage.value = file
  }
}

function selectDirectory(path: string) {
  directory.value = path
  browsing.value = false
}
</script>

<template>
  <el-dialog :model-value="true" :title="selected ? `在 ${target} 安装 ${selected.name}` : `在 ${target} 安装运行环境`" width="min(580px, 94vw)" append-to-body @close="emit('close')">
    <div v-if="!selected" class="template-grid" aria-label="运行环境模板">
      <button v-for="template in templates" :key="template.id" type="button" class="install-template-card" @click="selectTemplate(template)">
        <span class="template-title"><el-icon><component :is="template.icon" /></el-icon><strong>{{ template.name }}</strong></span>
        <span class="muted small">{{ template.description }}</span>
      </button>
    </div>

    <template v-else>
      <el-button :icon="ArrowLeft" text @click="selected = undefined">返回模板</el-button>
      <el-form label-position="top" @submit.prevent="submit">
        <el-form-item v-if="selected.id === 'dotnet'" label="安装组件">
          <el-radio-group v-model="component">
            <el-radio-button value="aspnetcore">ASP.NET Core Runtime</el-radio-button>
            <el-radio-button value="runtime">.NET Runtime</el-radio-button>
            <el-radio-button value="sdk">SDK</el-radio-button>
          </el-radio-group>
        </el-form-item>
        <el-form-item label="选择版本">
          <el-radio-group v-model="versionMode">
            <el-radio-button value="major" :disabled="!!localPackage">{{ selected.id === 'jdk' ? '大版本' : '版本系列' }}</el-radio-button>
            <el-radio-button value="version">精确版本</el-radio-button>
          </el-radio-group>
        </el-form-item>
        <el-form-item v-if="versionMode === 'major' && selected.id === 'jdk'" label="JDK 大版本">
          <el-input-number v-model="major" :min="8" :max="99" :precision="0" />
        </el-form-item>
        <el-form-item v-else-if="versionMode === 'major'" label="版本系列">
          <el-select v-model="channel">
            <el-option v-for="series in selected.channels" :key="series" :label="series" :value="series" />
          </el-select>
        </el-form-item>
        <el-form-item v-else label="完整版本">
          <el-input v-model="exactVersion" :placeholder="selected.id === 'dotnet' && component === 'sdk' ? '10.0.100' : selected.versionExample" />
        </el-form-item>
        <el-form-item label="安装位置">
          <el-radio-group v-model="locationMode">
            <el-radio-button value="shared">Ferry 公共目录</el-radio-button>
            <el-radio-button value="custom">自定义目录</el-radio-button>
          </el-radio-group>
          <div v-if="locationMode === 'shared'" class="shared-directory" aria-live="polite">
            <code v-if="sharedDirectory" class="mono">{{ sharedDirectory }}</code>
            <span v-else class="muted small">{{ sharedDirectoryError || '正在读取公共目录…' }}</span>
          </div>
        </el-form-item>
        <el-form-item v-if="locationMode === 'custom'" label="服务器目录" :error="directory ? directoryError(directory) : ''">
          <div class="directory-field">
            <el-input v-model="directory" class="mono" :placeholder="selected.directoryExample" clearable />
            <el-button :icon="FolderOpened" @click="browsing = true">浏览…</el-button>
          </div>
          <div class="muted small">Ferry 在所选目录下为每个版本建独立目录，应用可按路径选用。</div>
        </el-form-item>
        <el-form-item v-if="selected.id !== 'jdk'" label="安装包">
          <div class="directory-field">
            <el-input :model-value="localPackage" placeholder="本机缓存或发行源" readonly />
            <el-button :icon="FolderOpened" @click="selectPackage">选择本机包…</el-button>
            <el-button v-if="localPackage" @click="localPackage = ''">清除</el-button>
          </div>
        </el-form-item>
      </el-form>
      <p v-if="selected.id === 'jdk'" class="muted small install-note">Ferry 优先使用本机 JDK 库。同一大版本的多个补丁版本可以并存。</p>
      <p v-else class="muted small install-note">适用于使用 glibc 的 Linux x64 或 arm64。服务器架构会自动检测。</p>
    </template>

    <template #footer>
      <el-button @click="emit('close')">取消</el-button>
      <el-button v-if="selected" type="primary" :disabled="!canInstall" @click="submit">安装</el-button>
    </template>
  </el-dialog>
  <RemoteDirectoryPicker v-if="browsing" :config="config" :target="target" :path="installDir || '/opt'" :validate="directoryError" @select="selectDirectory" @close="browsing = false" />
</template>

<style scoped>
.template-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(200px, 1fr)); gap: 12px; }
.template-title { display: flex; align-items: center; gap: 8px; }
.el-form { margin-top: 12px; }
.shared-directory { width: 100%; line-height: 1.5; padding-top: 6px; overflow-wrap: anywhere; }
.directory-field { display: flex; gap: 8px; width: 100%; }
.install-note { margin: 0; }
</style>
