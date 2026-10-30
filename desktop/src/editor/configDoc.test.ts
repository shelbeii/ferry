import { describe, expect, it } from 'vitest'
import { addEntry, append, diffHunks, hasAlias, listAt, mapAt, parse, remove, renameKey, serialize, setList, setMap, setScalar, type Path } from './configDoc'
import type { Document } from 'yaml'
import shop from '../../../examples/shop/ferry.yaml?raw'
import e2e from '../../../test/e2e/ferry.yaml?raw'
import dirapps from '../../../test/e2e/dirapps.yaml?raw'

const src = `version: 1
project: demo   # 项目名
vars: {repo: .}
targets:
  t1:
    host: 10.0.0.1      # 测试机
    port: 22
apps:
  - name: api
    type: java
    artifact: {path: out/api.jar}
    service:
      jvmArgs: ["-Xmx1g"]
      health: {tcp: "8080"}
`

function edit(text: string, f: (d: Document) => void): string {
  const d = parse(text)
  f(d)
  return serialize(text, d)
}

describe('serialize', () => {
  it('没改的行保持原样', () => {
    expect(edit(src, () => {})).toBe(src)
  })

  it('改一个值只动那一行', () => {
    const out = edit(src, (d) => setScalar(d, ['targets', 't1', 'port'], 2222))
    expect(out).toBe(src.replace('port: 22', 'port: 2222'))
  })

  it('原来是数字的，填数字文字时不加引号', () => {
    const out = edit(src, (d) => setScalar(d, ['targets', 't1', 'port'], '2200'))
    expect(out).toContain('    port: 2200\n')
  })

  it('加字段加在对象末尾，其他行不变', () => {
    const out = edit(src, (d) => setScalar(d, ['targets', 't1', 'runAs'], 'app'))
    expect(out).toBe(src.replace('    port: 22\n', '    port: 22\n    runAs: app\n'))
  })

  it('新建嵌套对象', () => {
    const out = edit(src, (d) => setScalar(d, ['apps', 0, 'service', 'runtime', 'java'], 17))
    expect(out).toContain('      runtime:\n        java: 17\n')
    expect(out).toContain('vars: {repo: .}\n')
  })

  it('往 {} 里加东西改成块格式', () => {
    const t = 'version: 1\ntargets:\n  t1: {}\n'
    const out = edit(t, (d) => setScalar(d, ['targets', 't1', 'host'], 'h'))
    expect(out).toBe('version: 1\ntargets:\n  t1:\n    host: h\n')
  })

  it('删除后空了的上级一并删掉', () => {
    const t = 'apps:\n  - name: a\n    service:\n      health:\n        tcp: "1"\n'
    const out = edit(t, (d) => remove(d, ['apps', 0, 'service', 'health', 'tcp'], 2))
    expect(out).toBe('apps:\n  - name: a\n')
  })

  it('列表：保留一行的写法', () => {
    const out = edit(src, (d) => setList(d, ['apps', 0, 'service', 'jvmArgs'], ['-Xmx1g', '-Xms256m']))
    expect(out).toContain('      jvmArgs: ["-Xmx1g", -Xms256m]\n')
  })

  it('新列表短的写成一行', () => {
    const out = edit(src, (d) => setList(d, ['apps', 0, 'service', 'args'], ['--a', '--b']))
    expect(out).toContain('      args: [--a, --b]\n')
  })

  it('只写一个值的保持一个值', () => {
    const t = 'service:\n  envFile: /etc/a.env\n'
    expect(edit(t, (d) => setList(d, ['service', 'envFile'], ['/etc/b.env'], 0, true))).toBe('service:\n  envFile: /etc/b.env\n')
  })

  it('映射：改值、加键、删键', () => {
    const out = edit(src, (d) => setMap(d, ['vars'], [['repo', '..'], ['x', '1']]))
    expect(out).toContain('vars: {repo: .., x: "1"}\n')
    expect(mapAt(parse(out), ['vars'])).toEqual([['repo', '..'], ['x', '1']])
  })

  it('改键名位置不变', () => {
    const out = edit(src, (d) => renameKey(d, ['targets'], 't1', 'prod'))
    expect(out).toBe(src.replace('  t1:', '  prod:'))
  })

  it('加应用、加服务器', () => {
    const out = edit(src, (d) => {
      append(d, ['apps'], { name: 'web', type: 'static' })
      addEntry(d, ['targets'], 't2', { host: '10.0.0.2' })
    })
    expect(out).toContain('  t2:\n    host: 10.0.0.2\n')
    expect(out).toContain('  - name: web\n    type: static\n')
    expect(out).toContain('    host: 10.0.0.1      # 测试机\n')
  })

  it('列表读取', () => {
    const d = parse(src)
    expect(listAt(d, ['apps', 0, 'service', 'jvmArgs'])).toEqual(['-Xmx1g'])
  })

  it('别名、合并键', () => {
    const d = parse('a: &x {b: 1}\nc:\n  <<: *x\n  d: 2\ne: *x\n')
    const p = (...x: Path) => x
    expect(hasAlias(d, p('a'))).toBe(false)
    expect(hasAlias(d, p('c'))).toBe(true)
    expect(hasAlias(d, p('e'))).toBe(true)
  })
})

describe('diffHunks', () => {
  it('只显示改动附近', () => {
    const a = Array.from({ length: 20 }, (_, i) => `l${i}`).join('\n')
    const b = a.replace('l10', 'L10')
    const h = diffHunks(a, b, 2)
    expect(h).toHaveLength(1)
    expect(h[0].lines.map((l) => l.t + l.text)).toEqual([' l8', ' l9', '-l10', '+L10', ' l11', ' l12'])
  })
})

describe('仓库里的配置', () => {
  it('不改时原样写回，改一处只动一行', () => {
    for (const raw of [shop, e2e, dirapps]) {
      const t = raw.replace(/\r\n?/g, '\n')
      expect(edit(t, () => {})).toBe(t)
      const out = edit(t, (d) => setScalar(d, ['project'], 'renamed'))
      expect(diffHunks(t, out).flatMap((h) => h.lines.filter((l) => l.t !== ' '))).toHaveLength(2)
    }
  })
})
