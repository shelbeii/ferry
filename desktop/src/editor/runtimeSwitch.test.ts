import { describe,expect,it } from 'vitest'
import { parse } from './configDoc'
import { runtimeSwitchDraft } from './runtimeSwitch'
import type { AppRuntimeChoice } from '../ferry'

const source=`version: 1
project: demo # keep this
targets:
  prod: {host: 192.0.2.1, runAs: app}
  test: {host: 192.0.2.2, runAs: app, appRuntimes: {api: {java: /old/bin/java}}}
apps:
  - name: api
    type: java
    artifact: {path: api.jar}
    service: {runtime: {java: 17}}
`
const choice:AppRuntimeChoice={id:'java:/opt/jdk/bin/java',path:'/opt/jdk/bin/java',source:'服务器已安装'}
describe('切换运行环境配置',()=>{
  it('只覆盖所选服务器和应用，保留全局 JDK 设置及其他服务器',()=>{
    const out=runtimeSwitchDraft(source,'prod','api','java',choice)
    const doc=parse(out).toJS()
    expect(doc.targets.prod.appRuntimes.api).toEqual({java:'/opt/jdk/bin/java'})
    expect(doc.targets.test).toEqual(parse(source).toJS().targets.test)
    expect(doc.apps).toEqual(parse(source).toJS().apps)
    expect(out).toContain('project: demo # keep this')
  })
  it('保留 CRLF，替换旧覆盖时不遗留多种运行环境',()=>{
    const out=runtimeSwitchDraft(source.replaceAll('\n','\r\n'),'test','api','java',choice)
    expect(out).toContain('\r\n')
    expect(out.replaceAll('\r\n','')).not.toContain('\n')
    expect(parse(out).toJS().targets.test.appRuntimes.api).toEqual({java:choice.path})
  })
  it('Nginx 可同时选择实例和端口',()=>{
    const text=source.replace('type: java','type: static').replace('    service: {runtime: {java: 17}}','    static: {port: 8081}')
    const out=runtimeSwitchDraft(text,'prod','api','static',{id:'static:edge',path:'edge',source:'Ferry'},'8090')
    expect(parse(out).toJS().targets.prod.appRuntimes.api).toEqual({nginxInstance:'edge',port:'8090'})
  })
  it('拒绝禁用项、错误类型、错误端口和已变化的应用',()=>{
    expect(()=>runtimeSwitchDraft(source,'prod','api','java',{...choice,disabled:'不可执行'})).toThrow('不可执行')
    expect(()=>runtimeSwitchDraft(source,'prod','api','node',choice)).toThrow()
    expect(()=>runtimeSwitchDraft(source,'prod','missing','java',choice)).toThrow()
    expect(()=>runtimeSwitchDraft(source,'prod','api','java',{...choice,path:'relative/java'})).toThrow()
  })
})
