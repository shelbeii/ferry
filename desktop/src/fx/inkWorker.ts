// 取点的 Worker：把窗口里的文字、图标、控件画出来，找出有墨的像素（particleEngine.ts 的 inkPoints），
// 直接经 MessageChannel 交给画粒子的 Worker（particleWorker.ts）。
// 单独一个线程：取点要十几到几十毫秒，放在画粒子的线程里会让粒子停一帧；结果也不经主线程转（主线程这时常常正忙着生成窗口内容）
import { inkPoints, warmUp } from './particleEngine'
import type { Shape } from './contentShapes'

export type ToInk =
  | { type: 'port'; port: MessagePort }
  | { type: 'ink'; id: number; shapes: Shape[]; w: number; h: number }

let port: MessagePort | null = null

self.onmessage = (e: MessageEvent<ToInk>) => {
  const m = e.data
  if (m.type === 'port') {
    port = m.port
    try { warmUp() } catch { /* 预热失败不影响 */ }
  } else if (m.type === 'ink') {
    const ink = inkPoints(m.shapes, m.w, m.h)
    if (ink && port) port.postMessage({ id: m.id, ink }, [ink.idx.buffer])
  }
}
