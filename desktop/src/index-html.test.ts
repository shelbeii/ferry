import { describe, expect, it } from 'vitest'
import html from '../index.html?raw'

describe('index.html', () => {
  // 桌面版会给 <style> 加 nonce，CSP 里有 nonce 后 'unsafe-inline' 失效：编辑器、终端运行时插入的样式被拦住，编辑配置一片空白
  it('没有 <style>', () => {
    expect(html).not.toMatch(/<style[\s>]/i)
  })
})
