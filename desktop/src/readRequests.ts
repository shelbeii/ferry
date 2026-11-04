import { getCurrentScope, onScopeDispose, ref } from 'vue'
import { ferry, FerryError } from './ferry'

export class ReadCanceled extends Error {
  constructor() { super('已取消读取') }
}

/** 只读请求按目标独立等待；取消/超时不依赖后台命令能否及时收尾。 */
export function useReadRequests() {
  const loading = ref<Record<string, boolean>>({})
  const entries = new Map<string, { cancel: () => void; promise: Promise<unknown> }>()

  function cancel(key: string) {
    const entry = entries.get(key)
    if (!entry) return
    entries.delete(key)
    delete loading.value[key]
    entry.cancel()
  }

  function run<T>(key: string, read: (signal: AbortSignal) => Promise<T>, options: {
    signal?: AbortSignal
    timeout?: number
    timeoutMessage?: string
  } = {}): Promise<T> {
    if (options.signal?.aborted) return Promise.reject(new ReadCanceled())
    const existing = entries.get(key)
    if (existing) return existing.promise as Promise<T>
    const controller = new AbortController()
    const signal = controller.signal
    const abort = () => controller.abort(new ReadCanceled())
    options.signal?.addEventListener('abort', abort, { once: true })
    const timeout = options.timeout ?? 60_000
    const timer = setTimeout(() => controller.abort(new Error(options.timeoutMessage ?? `读取超时（${timeout / 1000} 秒），请检查网络后重试`)), timeout)
    const aborted = new Promise<never>((_, reject) => signal.addEventListener('abort', () => reject(signal.reason), { once: true }))
    const entry = { cancel: abort, promise: undefined as unknown as Promise<T> }
    loading.value[key] = true
    entry.promise = Promise.race([
      Promise.resolve().then(() => {
        if (signal.aborted) throw signal.reason
        return read(signal)
      }),
      aborted,
    ]).finally(() => {
      clearTimeout(timer)
      options.signal?.removeEventListener('abort', abort)
      if (entries.get(key) === entry) {
        entries.delete(key)
        delete loading.value[key]
      }
    })
    entries.set(key, entry)
    return entry.promise
  }

  const cancelAll = () => { for (const key of entries.keys()) cancel(key) }
  if (getCurrentScope()) onScopeDispose(cancelAll)
  return { loading, run, cancel, cancelAll }
}

/** 取消只结束本次读取，不断开共享 SSH 连接，也不终止部署等其他请求。 */
export async function readCall<T>(args: string[], signal: AbortSignal, env: Record<string, string> = {}): Promise<T> {
  if (signal.aborted) throw signal.reason
  const proc = ferry<T>(args, undefined, env)
  let stop!: () => void
  const aborted = new Promise<never>((_, reject) => {
    stop = () => {
      reject(signal.reason)
      void proc.kill().catch(() => undefined)
    }
    signal.addEventListener('abort', stop, { once: true })
  })
  try {
    const result = await Promise.race([proc.done, aborted])
    if (signal.aborted) throw signal.reason
    if (!result.ok) throw new FerryError(result.error ?? '读取失败', result.detail)
    return result.data as T
  } finally {
    signal.removeEventListener('abort', stop)
  }
}
