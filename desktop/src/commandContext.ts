const direct = new Set(['deploy', 'start', 'stop', 'restart', 'rollback', 'status', 'preflight', 'changes', 'app-info', 'app-runtimes', 'server-info', 'logs', 'backup', 'monitor', 'keygen', 'trust', 'hostkey', 'ai-exec'])
const softwareActions = new Set(['install', 'start', 'stop', 'restart', 'remove', 'restore', 'upgrade', 'backup', 'logs', 'password'])
const softwareLocal = new Set(['templates', 'template', 'latest', 'lib'])
const portActions = new Set(['open', 'close', 'enable', 'disable', 'check'])

/** 桌面版发出的命令所操作的服务器，供任务归属、认证重试和 AI 排查共同使用。 */
export function commandTarget(args: string[]): string | undefined {
  let index: number
  if (direct.has(args[0])) index = 1
  else if (args[0] === 'software') {
    if (softwareLocal.has(args[1])) return undefined
    index = args[1] === 'config' ? 3 : softwareActions.has(args[1]) ? 2 : 1
  } else if (args[0] === 'port') index = portActions.has(args[1]) ? 2 : 1
  else if (args[0] === 'who') index = args[1] === 'kick' ? 2 : 1
  else if (args[0] === 'files') index = 2
  else return undefined
  const target = args[index]
  return target && !target.startsWith('-') ? target.split(',')[0] : undefined
}
