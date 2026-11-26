import { describe, expect, it } from 'vitest'
import { commandTarget } from './commandContext'

describe('任务与认证的目标解析', () => {
  it.each(['install', 'start', 'stop', 'restart', 'remove', 'restore', 'upgrade', 'backup', 'logs', 'password'])('software %s 始终归属真实服务器', (action) => {
    expect(commandTarget(['software', action, 'tunnel-41', 'redis', '-c', 'ferry.yaml'])).toBe('tunnel-41')
  })
  it.each(['list', 'associate', 'read', 'save', 'apply', 'restore'])('software config %s 不把子命令认成服务器', (op) => {
    expect(commandTarget(['software', 'config', op, 'tunnel-162', '--unit', 'nginx.service'])).toBe('tunnel-162')
  })
  it('为 JDK 清单、安装、卸载和同名服务器解析正确目标', () => {
    expect(commandTarget(['software', 'jdk', 'list', 'prod'])).toBe('prod')
    expect(commandTarget(['software', 'jdk', 'install', 'prod', '--java', '21'])).toBe('prod')
    expect(commandTarget(['software', 'jdk', 'remove', 'prod', '/opt/jdks/jdk-21-x64/bin/java', '--yes'])).toBe('prod')
    expect(commandTarget(['software', 'runtime', 'install', 'prod', 'node', '--channel', '24'])).toBe('prod')
    expect(commandTarget(['software', 'jdk', '-c', 'ferry.yaml'])).toBe('jdk')
  })
  it('覆盖多目标、端口测试、防火墙安装卸载认证、会话、备份、文件与免密配置', () => {
    for (const args of [
      ['deploy', '41,162', 'api'],
      ['port', 'check', '41', '8080'],
      ['port', 'install', '41', 'firewalld', '--enable'],
      ['port', 'uninstall', '41', 'firewalld', '--purge'],
      ['port', 'uninstall-auth', '41', 'firewalld'],
      ['port', 'uninstall-challenge', '41', 'firewalld'],
      ['who', 'kick', '41', '123'],
      ['backup', '41', 'restore'],
      ['files', 'ls', '41', '/'],
      ['keygen', '41'],
      ['mcp-exec', '41', '-c', 'ferry.yaml'],
      ['mcp-exec', '41,162', '-c', 'ferry.yaml'],
    ]) expect(commandTarget(args)).toBe('41')
    expect(commandTarget(['software', '41', '-c', 'ferry.yaml'])).toBe('41')
  })
  it.each([['runtime', 'list'], ['software', 'latest', 'nginx'], ['software', 'lib', 'import', 'nginx.tar'], ['software', 'template', 'save'], ['settings'], ['status', '-c', 'ferry.yaml'], ['mcp-exec'], ['mcp-exec', '-c', 'ferry.yaml']])('本地命令不产生虚构服务器 %j', (...args) => {
    expect(commandTarget(args)).toBeUndefined()
  })
})
