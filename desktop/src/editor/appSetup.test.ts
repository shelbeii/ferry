import { describe, expect, it } from 'vitest'
import { applyAppSetup, setupError, type AppSetup } from './appSetup'
import { parse, serialize } from './configDoc'

const source = `version: 1
project: shop
vars: {nginx_web: old-value}
targets:
  prod: {host: 10.0.0.1, runAs: app, apps: [api]}
  test: {host: 10.0.0.2, runAs: app}
  other: {host: 10.0.0.3, runAs: app, apps: [api]}
apps:
  - {name: api, type: java, artifact: {path: api.jar}}
`
function setup(extra: Partial<AppSetup> = {}): AppSetup {
  return { name: 'web', type: 'static', path: 'dist', targets: ['prod'], build: '', buildCommand: '', buildWorkdir: '', user: '',
    javaSource: 'ferry', javaMajor: 17, javaPaths: {}, jvmArgs: '', args: '', exec: '', env: '',
    port: '8081', serve: 'installed', instances: { prod: 'edge', test: 'system:nginx.service' }, domains: 'web.example.test, www.example.test',
    spa: true, proxies: [{ path: '/api/', to: 'http://127.0.0.1:8080/' }], ...extra }
}

describe('新增应用的运行配置', () => {
  it('已安装 nginx 生成可托管的站点，显式应用范围只加入所选服务器', () => {
    const doc = parse(source)
    applyAppSetup(doc, setup())
    const out = parse(serialize(source, doc)).toJS()
    expect(out.apps[1]).toEqual({ name: 'web', type: 'static', artifact: { path: 'dist' }, static: { nginxInstance: 'edge', port: '8081', spa: true,
      domains: ['web.example.test', 'www.example.test'], proxy: { '/api/': 'http://127.0.0.1:8080/' } } })
    expect(out.targets.prod.apps).toEqual(['api', 'web'])
    expect(out.targets.test.apps).toEqual(['api'])
    expect(out.targets.other.apps).toEqual(['api'])
    expect(out.apps[0]).toEqual(parse(source).toJS().apps[0])
  })

  it('多服务器使用不同 nginx 时生成独立变量，避开已有变量', () => {
    const doc = parse(source)
    applyAppSetup(doc, setup({ targets: ['prod', 'test'] }))
    const out = doc.toJS()
    expect(out.apps[1].static.nginxInstance).toBe('${nginx_web_2}')
    expect(out.vars.nginx_web).toBe('old-value')
    expect(out.targets.prod.vars.nginx_web_2).toBe('edge')
    expect(out.targets.test.vars.nginx_web_2).toBe('system:nginx.service')
    expect(out.targets.other.vars).toBeUndefined()
  })

  it('Java 可以分别绑定各服务器的已有 JDK，并保留程序参数和环境变量中的等号', () => {
    const doc = parse(source)
    applyAppSetup(doc, setup({ type: 'java', targets: ['prod', 'test'], path: 'api.jar', javaSource: 'server',
      javaPaths: { prod: '/opt/jdk/bin/java', test: '/usr/bin/java' }, args: '--spring.profiles.active=prod', jvmArgs: '-Xmx512m', env: 'OPTIONS=a=b\nPORT=8080' }))
    const service = doc.toJS().apps[1].service
    expect(service.java).toBe('${java_web}')
    expect(service.runtime).toBeUndefined()
    expect(service.args).toEqual(['--spring.profiles.active=prod'])
    expect(service.env).toEqual({ OPTIONS: 'a=b', PORT: '8080' })
  })

  it('自动 JDK、目录应用启动命令与构建命令可直接生成', () => {
    const doc = parse(source)
    applyAppSetup(doc, setup({ type: 'java', build: '@inline', buildCommand: 'mvn package', buildWorkdir: 'backend', user: 'ferry' }))
    expect(doc.toJS().apps[1].service.runtime).toEqual({ java: 17 })
    expect(doc.toJS().apps[1].build).toEqual({ command: 'mvn package', workdir: 'backend' })
    const node = parse(source)
    applyAppSetup(node, setup({ type: 'node', exec: 'node\n${running}/server.js', port: '3000', user: 'ferry' }))
    expect(node.toJS().apps[1].service).toEqual({ user: 'ferry', exec: ['node', '${running}/server.js'], health: { tcp: '3000' } })
  })

  it('明确区分独立运行和仅发布文件', () => {
    const standalone = parse(source)
    applyAppSetup(standalone, setup({ serve: 'standalone', user: 'ferry' }))
    expect(standalone.toJS().apps[1].static.nginxInstance).toBeUndefined()
    expect(standalone.toJS().apps[1].service.user).toBe('ferry')
    const files = parse(source)
    applyAppSetup(files, setup({ serve: 'files' }))
    expect(files.toJS().apps[1].static).toBeUndefined()
    expect(files.toJS().apps[1].service).toBeUndefined()
    expect(setupError(parse(source), setup({ env: '从其他类型保留的无效环境变量' }))).toBe('')
  })

  it('缺少启动条件、重复应用、错误端口和代理都在添加前拦截', () => {
    const doc = parse(source)
    for (const extra of [
      { name: 'API' }, { path: '' }, { targets: [] }, { instances: {} }, { port: '65536' },
      { proxies: [{ path: 'api', to: 'localhost:8080' }] }, { type: 'node' as const, exec: '' },
      { type: 'java' as const, javaSource: 'server' as const, javaPaths: {} }, { type: 'binary' as const, user: 'root' },
    ]) expect(setupError(doc, setup(extra))).not.toBe('')
    expect(setupError(doc, setup({ port: '[::1]:8081' }))).toBe('')
  })
})
