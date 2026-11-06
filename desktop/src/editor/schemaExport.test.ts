// 把结构说明导出成 internal/config/schema.json：Ferry 的 AI 助手、编程软件（MCP 的 config_schema）用它查字段的意思。
// 内容和 schema.ts 不一致时测试失败；改了 schema.ts 后用 FERRY_UPDATE_SCHEMA=1 npx vitest run src/editor/schemaExport.test.ts 重新生成。
import { describe, expect, it } from 'vitest'
import { builtinVars, schema, type SchemaNode } from './schema'
import current from '../../../internal/config/schema.json?raw'

type Out = Omit<SchemaNode, 'fields' | 'map' | 'item'> & { name?: string; fields?: Out[]; map?: Out; item?: Out }

// fields 写成数组：Go 读 JSON 对象时不保留顺序，表单里的先后对 AI 也有用
function convert(n: SchemaNode, name?: string): Out {
  const { fields, map, item, ...rest } = n
  const out: Out = name === undefined ? { ...rest } : { name, ...rest }
  if (fields) out.fields = Object.entries(fields).map(([k, c]) => convert(c, k))
  if (map) out.map = convert(map)
  if (item) out.item = convert(item)
  return out
}

describe('schema.json', () => {
  it('和 schema.ts 一致', async () => {
    const want = JSON.stringify({ schema: convert(schema), builtinVars }, null, 1) + '\n'
    const env = (globalThis as { process?: { env: Record<string, string | undefined> } }).process?.env ?? {}
    if (env.FERRY_UPDATE_SCHEMA === '1') {
      // 只在重新生成时用到 Node 的 fs（桌面版的类型检查里没有 Node 的类型）
      const fsName = 'node:fs'
      const fs = (await import(/* @vite-ignore */ fsName)) as { writeFileSync(p: URL, s: string): void }
      fs.writeFileSync(new URL('../../../internal/config/schema.json', import.meta.url), want)
      return
    }
    expect(current.replace(/\r\n/g, '\n'), '改了 schema.ts 要重新生成 internal/config/schema.json（FERRY_UPDATE_SCHEMA=1）').toBe(want)
  })
})
