import { computed, ref, watch } from 'vue'
import type { Connection } from './ferry'
import { setUiPref, uiPref } from './state'

export type AiAccessServer = Pick<Connection, 'host' | 'port' | 'user' | 'hostKey'>
const autoLevels = ['warning', 'danger'] as const
type AutoLevel = typeof autoLevels[number]

interface AiAccessTarget {
  config: string
  target: string
  server?: AiAccessServer
  production?: boolean
  noChanges?: boolean
}

interface Preferences {
  allowChange: boolean
  shareData: boolean
  autoConfirm: AutoLevel[]
}

function savedLevels(value: unknown): AutoLevel[] {
  return Array.isArray(value) ? autoLevels.filter((level) => value.includes(level)) : []
}

function storedAccess(key: string): Preferences {
  try {
    const value = key ? JSON.parse(uiPref(key) ?? 'null') as Partial<Preferences> | null : null
    return { allowChange: value?.allowChange === true, shareData: value?.shareData === true, autoConfirm: savedLevels(value?.autoConfirm) }
  } catch {
    return { allowChange: false, shareData: false, autoConfirm: [] }
  }
}

export function useAiAccess(target: AiAccessTarget, isOpen: () => boolean) {
  const key = computed(() => {
    const { config, target: name, server } = target
    return server ? 'ai.access.' + JSON.stringify([config, name, server.host, server.port, server.user, server.hostKey]) : ''
  })
  const preferences = ref(storedAccess(key.value))
  watch(key, (value) => { preferences.value = storedAccess(value) }, { flush: 'sync' })

  const productionChange = ref(false)
  watch([key, isOpen, () => target.production, () => target.noChanges], () => {
    productionChange.value = false
  }, { flush: 'sync' })

  function save(update: Partial<Preferences>) {
    preferences.value = { ...preferences.value, ...update }
    if (key.value) setUiPref(key.value, JSON.stringify(preferences.value))
  }

  const allowChange = computed({
    get: () => !target.noChanges && (target.production ? productionChange.value : preferences.value.allowChange),
    set: (value: boolean) => {
      if (target.noChanges) return
      if (target.production) productionChange.value = value
      else save({ allowChange: value })
    },
  })
  const shareData = computed({
    get: () => preferences.value.shareData,
    set: (value: boolean) => save({ shareData: value }),
  })
  const autoConfirm = computed({
    get: () => preferences.value.autoConfirm,
    set: (value: AutoLevel[]) => save({ autoConfirm: savedLevels(value) }),
  })
  return { allowChange, shareData, autoConfirm }
}
