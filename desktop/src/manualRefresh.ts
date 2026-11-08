import { computed, ref } from 'vue'
import { ElMessage } from 'element-plus'

// 点「应用 / 软件 / 端口 / 登录用户 / 服务器信息」各自工具栏上的刷新：只重新读这一页的数据。
// 读取期间按钮转圈、这一页的内容变淡（按实际读取的时间），读完提示「××已刷新」（读取失败时下面有出错的提示，不再提示成功）。
// 右上角的刷新是整页的（重新读配置、连接、应用状态和当前页），盖一层「正在刷新」，和这里区分开。
export function useManualRefresh(key: () => string) {
  const busy = ref<Record<string, boolean>>({})
  const refreshing = computed(() => !!busy.value[key()])
  async function run(label: string, load: () => Promise<unknown>, ok: () => boolean) {
    const k = key()
    if (busy.value[k]) return
    busy.value[k] = true
    try {
      await load()
    } finally {
      delete busy.value[k]
    }
    if (key() === k && ok()) ElMessage.success({ message: `${label}已刷新`, duration: 1500 })
  }
  return { refreshing, run }
}
