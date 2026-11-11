// 文件列表里「查看大小」的结果放在面板里（按完整路径），不放在每行的格子里：
// 列表只画看得见的行，滚出去的行会被卸掉，结果和正在进行的统计不能跟着丢。
import { reactive } from 'vue'
import { ferry, type FileEntry, type RemoteFileSize, type Running } from './ferry'

export interface SizeState {
  /** 统计时这个条目的修改时间和大小；变了就当没统计过 */
  stamp: string
  loading: boolean
  result?: RemoteFileSize
  error: string
}

const stampOf = (entry: Pick<FileEntry, 'mtime' | 'size'>) => `${entry.mtime}|${entry.size}`

export function useFileSizes() {
  const states = reactive(new Map<string, SizeState>())
  const running = new Map<string, Running<RemoteFileSize>>()

  function get(path: string, entry: FileEntry): SizeState | undefined {
    const state = states.get(path)
    return state && state.stamp === stampOf(entry) ? state : undefined
  }

  async function measure(config: string, target: string, path: string, entry: FileEntry) {
    const current = get(path, entry)
    if (current?.loading) return
    void running.get(path)?.kill()
    if (!current) states.set(path, { stamp: stampOf(entry), loading: false, error: '' })
    const state = states.get(path)!
    state.loading = true
    state.error = ''
    const proc = ferry<RemoteFileSize>(['files', 'size', target, path, '-c', config])
    running.set(path, proc)
    try {
      const response = await proc.done
      if (running.get(path) !== proc) return
      if (!response.ok || !response.data) throw new Error(response.error || '大小统计失败')
      state.result = response.data
    } catch (e) {
      if (running.get(path) === proc) state.error = (e as Error).message
    } finally {
      if (running.get(path) === proc) {
        running.delete(path)
        state.loading = false
      }
    }
  }

  function cancel(path: string) {
    const proc = running.get(path)
    if (!proc) return
    running.delete(path)
    void proc.kill()
    const state = states.get(path)
    if (state) state.loading = false
  }

  /** 换目录、换服务器、关掉面板：停掉所有统计，结果清空 */
  function clear() {
    for (const proc of running.values()) void proc.kill()
    running.clear()
    states.clear()
  }

  return { get, measure, cancel, clear }
}
