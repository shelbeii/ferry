import { customRef, type Ref } from 'vue'
const requestKeys = new WeakMap<object, number>()
let nextRequestKey = 0
const completed = new WeakSet<object>()
export function completeRequest(request: object) { completed.add(request) }
export function mergeRetries(first: { retry?: () => void }, next: { retry?: () => void }) {
  const before = first.retry, after = next.retry
  if (!after || before === after) return
  first.retry = () => { before?.(); after() }
}
export function requestKey(request: object) {
  if (!requestKeys.has(request)) requestKeys.set(request, ++nextRequestKey)
  return requestKeys.get(request)!
}

/** 并发任务的认证/确认请求依次展示；关闭当前请求只移除当前项。 */
export function queuedRequest<T extends object>(options?: { key: (request: T) => string; merge: (first: T, next: T) => void }): Ref<T | null> {
  const queue: T[] = []
  return customRef((track, trigger) => ({
    get() { track(); return queue[0] ?? null },
    set(value: T | null) {
      if (value === null) { if (queue.length) { queue.shift(); trigger() } }
      else {
        const duplicate = options && queue.find(item => !completed.has(item) && options.key(item) === options.key(value))
        if (duplicate) { options!.merge(duplicate, value); trigger() }
        else { queue.push(value); if (queue.length === 1) trigger() }
      }
    },
  }))
}
