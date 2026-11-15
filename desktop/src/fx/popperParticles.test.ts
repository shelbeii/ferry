import { afterEach, expect, it, vi } from 'vitest'
import { installPopperParticles } from './popperParticles'

vi.mock('./dialogParticles', () => ({
  CLOSE_MS: 110,
  OPEN_MS: 300,
  particlesIn: () => true,
  particlesOut: () => true,
  slow: () => 1,
}))

afterEach(() => vi.unstubAllGlobals())

function pendingTransition() {
  let finish!: () => void
  const finished = new Promise<void>((resolve) => { finish = resolve })
  return { finished, finish }
}

function setup() {
  class Transition {
    private done = pendingTransition()
    finished = this.done.finished
    finish = this.done.finish
  }
  const transition = new Transition()
  const enter = { ...pendingTransition(), cancel: vi.fn() }
  const leave = { ...pendingTransition(), cancel: vi.fn() }
  class Popper {
    classList = ['el-fade-in-linear-leave-active']
    matches = () => true
    getAnimations = () => [transition]
    animate = vi.fn().mockReturnValueOnce(leave).mockReturnValueOnce(enter)
  }
  const popper = new Popper()
  let listener: (event: { target: Popper }) => void
  vi.stubGlobal('HTMLElement', Popper)
  vi.stubGlobal('CSSTransition', Transition)
  vi.stubGlobal('document', {
    addEventListener: (_: string, handle: typeof listener) => { listener = handle },
  })
  installPopperParticles()
  return { popper, transition, leave, enter, run: () => listener({ target: popper }) }
}

it('releases the closing opacity after the CSS transition hides a persistent popover', async () => {
  const { transition, leave, run } = setup()
  run()
  leave.finish()
  await leave.finished
  expect(leave.cancel).not.toHaveBeenCalled()

  transition.finish()
  await vi.waitFor(() => expect(leave.cancel).toHaveBeenCalledOnce())
})

it('keeps the new animation when the popover is reopened during its close transition', async () => {
  const { popper, transition, leave, enter, run } = setup()
  run()
  popper.classList = ['el-fade-in-linear-enter-active']
  run()
  transition.finish()
  leave.finish()
  await Promise.all([transition.finished, leave.finished])

  expect(popper.animate).toHaveBeenCalledTimes(2)
  expect(leave.cancel).toHaveBeenCalledOnce()
  expect(enter.cancel).not.toHaveBeenCalled()
})
