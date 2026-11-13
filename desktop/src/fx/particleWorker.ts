// 在 Web Worker 里算、画粒子（见 particleEngine.ts）：主线程只发窗口的位置过来，忙着生成界面时粒子照样一帧不落。
import { createEngine, type Box, type EffectSpec, type Engine, type Ink, type View } from './particleEngine'
import type { Shape } from './contentShapes'

export type ToWorker =
  | { type: 'init'; canvas: OffscreenCanvas }
  | { type: 'start'; gen: number; spec: EffectSpec; view: View }
  | { type: 'track'; id: number; rect: Box; view: View }
  | { type: 'retarget'; id: number; shapes: Shape[] }
  | { type: 'inkPort'; port: MessagePort }
export type FromWorker =
  | { type: 'ready'; ok: boolean }
  | { type: 'idle'; gen: number; frames: number; maxGap: number; inkMs: number }

const post = (m: FromWorker) => (self as unknown as Worker).postMessage(m)
let engine: Engine | null = null
let gen = 0

self.onmessage = (e: MessageEvent<ToWorker>) => {
  const m = e.data
  if (m.type === 'init') {
    engine = createEngine(m.canvas, stats => post({ type: 'idle', gen, ...stats }))
    post({ type: 'ready', ok: !!engine })
  } else if (m.type === 'start') {
    gen = m.gen
    engine?.start(m.spec, m.view)
  } else if (m.type === 'track') {
    engine?.track(m.id, m.rect, m.view)
  } else if (m.type === 'retarget') {
    engine?.retarget(m.id, m.shapes)
  } else if (m.type === 'inkPort') {
    // 取点的 Worker（inkWorker.ts）算好的落点直接从这里来
    m.port.onmessage = (ev: MessageEvent<{ id: number; ink: Ink }>) => engine?.applyInk(ev.data.id, ev.data.ink)
  }
}
