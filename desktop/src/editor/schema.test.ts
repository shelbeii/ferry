import { describe, expect, it } from 'vitest'
import { fieldPaths, lookup, schema, type SchemaNode } from './schema'
// Go 的配置结构生成的字段列表（internal/config/fields_test.go）
import fieldsTxt from '../../../internal/config/testdata/fields.txt?raw'

const goFields = fieldsTxt
  .split(/\r?\n/)
  .filter(Boolean)

describe('schema', () => {
  it('字段和 Go 的配置结构一致', () => {
    expect(fieldPaths()).toEqual(goFields)
  })

  it('每个字段都有名字、说明和输入方式', () => {
    const bad: string[] = []
    const walk = (n: SchemaNode, at: string) => {
      for (const [k, c] of Object.entries(n.fields ?? {})) {
        const p = `${at}${k}`
        if (!c.label || !c.kind) bad.push(p)
        walk(c, p + '.')
        if (c.map && c.kind === 'objMap') walk(c.map, p + '.*.')
        if (c.item) walk(c.item, p + '.[].')
      }
    }
    walk(schema, '')
    expect(bad).toEqual([])
  })

  it('二选一、needs、unless 指向存在的字段', () => {
    const bad: string[] = []
    const walk = (n: SchemaNode, at: string) => {
      const f = n.fields ?? {}
      for (const g of n.oneOf ?? []) for (const k of g) if (!f[k]) bad.push(`${at} oneOf ${k}`)
      for (const [k, c] of Object.entries(f)) {
        if (c.needs && !f[c.needs]) bad.push(`${at}${k} needs ${c.needs}`)
        if (c.unless && !f[c.unless]) bad.push(`${at}${k} unless ${c.unless}`)
        if (c.short && c.short !== '@build' && !c.fields?.[c.short]) bad.push(`${at}${k} short ${c.short}`)
        walk(c, `${at}${k}.`)
        if (c.map) walk(c.map, `${at}${k}.*.`)
        if (c.item) {
          if (c.item.short && !c.item.fields?.[c.item.short]) bad.push(`${at}${k}[] short ${c.item.short}`)
          walk(c.item, `${at}${k}.[].`)
        }
      }
    }
    walk(schema, '')
    expect(bad).toEqual([])
  })

  it('lookup 按路径找到说明', () => {
    expect(lookup(['targets', 'any', 'host'])?.required).toBe(true)
    expect(lookup(['apps', '[]', 'service', 'exec'])?.required).toEqual(['python', 'node', 'dotnet'])
    expect(lookup(['backups', '[]'])?.oneOf?.[0]).toContain('mysql')
  })
})
