import { describe, expect, it } from 'vitest'
import { commandTarget } from './commandContext'

describe('任务与认证的目标解析', () => {
  it.each(['install', 'start', 'stop', 'restart', 'remove', 'restore', 'upgrade', 'backup', 'logs', 'password'])('software %s 始终归属真实服务器', (action) => {
    expect(commandTarget(['software', action, 'tunnel-41', 'redis', '-c', 'ferry.yaml'])).toBe('tunnel-41')
  })
  it.each(['list', 'associate', 'read', 'save', 'apply', 'restore'])('software config %s 不把子命令认成服务器', (op) => {
    expect(commandTarget(['software', 'config', op, 'tunnel-162', '--unit', 'nginx.service'])).toBe('tunnel-162')
  })
  it('覆盖多目标、端口测试、会话、备份、文件与免密配置', () => {
    for (const args of [['deploy', '41,162', 'api'], ['port', 'check', '41', '8080'], ['who', 'kick', '41', '123'], ['backup', '41', 'restore'], ['files', 'ls', '41', '/'], ['keygen', '41']]) expect(commandTarget(args)).toBe('41')
    expect(commandTarget(['software', '41', '-c', 'ferry.yaml'])).toBe('41')
  })
  it.each([['runtime', 'list'], ['software', 'latest', 'nginx'], ['software', 'lib', 'import', 'nginx.tar'], ['software', 'template', 'save'], ['settings'], ['status', '-c', 'ferry.yaml']])('本地命令不产生虚构服务器 %j', (...args) => {
    expect(commandTarget(args)).toBeUndefined()
  })
})
