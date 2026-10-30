import { describe, expect, it } from 'vitest'
import { __test } from './yamlEditor'
import { lookup } from './schema'

const { pathAt, mapErrors, locate } = __test

const doc = `version: 1
project: shop
vars:
  repo: .
targets:
  prod-a:
    host: 203.0.113.10
    auth: password
apps:
  - name: System
    type: java
    service: &svc
      unit: shop-system
      health: {tcp: "9201"}
      unitExtra:
        service:
          - X=1
  - name: File
    service:
      <<: *svc
      health:
        tcp: "9300"
`
const lines = doc.split('\n')
const at = (needle: string) => {
  const i = lines.findIndex((l) => l.includes(needle))
  const l = lines[i]
  const indent = l.length - l.trimStart().length
  const isItem = l.trimStart().startsWith('- ')
  return pathAt(lines, i, isItem ? indent + 2 : indent, isItem)
}

describe('pathAt', () => {
  it('顶层与映射', () => {
    expect(at('project:')).toEqual([])
    expect(at('repo:')).toEqual(['vars'])
    expect(at('host: 203')).toEqual(['targets', 'prod-a'])
  })
  it('列表元素', () => {
    expect(at('- name: System')).toEqual(['apps', '[]'])
    expect(at('type: java')).toEqual(['apps', '[]'])
    expect(at('unit: shop-system')).toEqual(['apps', '[]', 'service'])
    expect(at('unitExtra:')).toEqual(['apps', '[]', 'service'])
    expect(at('tcp: "9300"')).toEqual(['apps', '[]', 'service', 'health'])
  })
  it('路径能找到结构说明', () => {
    expect(lookup([...at('host: 203'), 'auth'])?.values).toEqual(['key', 'password'])
    expect(lookup([...at('type: java'), 'type'])?.values).toContain('static')
    expect(lookup(at('tcp: "9300"'))?.fields?.timeout).toBeTruthy()
    expect(lookup(['targets', 'any-name'])?.fields?.identityFile).toBeTruthy()
  })
})

describe('mapErrors', () => {
  it('YAML 解码错误按行号', () => {
    const d = mapErrors('C:\\x\\ferry.yaml: yaml: unmarshal errors:\n  line 7: field hots not found in type config.Target', doc)
    expect(d).toHaveLength(1)
    expect(doc.slice(d[0].from, d[0].to)).toBe('    host: 203.0.113.10')
  })
  it('按字段路径定位', () => {
    const d = mapErrors('C:\\x\\ferry.yaml:\ntargets.prod-a：host 不能为空\napps.File：type 只能是 java、binary、static\nproject "GZ" 无效', doc)
    expect(d).toHaveLength(3)
    expect(doc.slice(d[0].from, d[0].from + 6)).toBe('prod-a')
    expect(doc.slice(d[1].from, d[1].from + 10)).toBe('name: File')
    expect(doc.slice(d[2].from, d[2].from + 7)).toBe('project')
  })
})

describe('locate', () => {
  it('找到值', () => {
    const l = locate(doc, ['targets', 'prod-a', 'host'])!
    expect(l.found).toBe(true)
    expect(doc.slice(l.from, l.to)).toBe('203.0.113.10')
  })
  it('没设置时停在最近的上级', () => {
    const l = locate(doc, ['targets', 'prod-a', 'root'])!
    expect(l.found).toBe(false)
    expect(doc.slice(l.from, l.to)).toBe('prod-a')
  })
  it('目标不存在时停在 targets', () => {
    const l = locate(doc, ['targets', 'nope', 'root'])!
    expect(l.found).toBe(false)
    expect(doc.slice(l.from, l.to)).toBe('targets')
  })
})
