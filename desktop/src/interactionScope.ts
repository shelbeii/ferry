import { onScopeDispose, watch } from 'vue'

/** 确认框、文件选择器等等待期间，页面换过目标或已销毁，就放弃尚未发出的操作。 */
export function useInteractionScope(identity: () => unknown) {
  let revision = 0
  let disposed = false
  watch(identity, () => { revision++ }, { flush: 'sync' })
  onScopeDispose(() => { disposed = true; revision++ })
  return () => {
    const captured = revision
    return () => !disposed && captured === revision
  }
}
