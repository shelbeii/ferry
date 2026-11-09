import { isSeq } from 'yaml'
import { hasAlias, nodeAt, parse, scalarAt, serialize, setValue } from './configDoc'
import type { AppRuntimeChoice } from '../ferry'

const fields: Record<string,string> = { java:'java', static:'nginxInstance', python:'python', node:'node', dotnet:'dotnet' }

/** 只写当前服务器的当前应用，保留其他目标、应用和无关的 YAML 内容。 */
export function runtimeSwitchDraft(raw: string, target: string, app: string, type: string, choice: AppRuntimeChoice, port = ''): string {
  if (choice.disabled) throw new Error(choice.disabled)
  const site = type === 'static' && choice.id.startsWith('static:site:')
  if (site && (!choice.path.startsWith('/') || /[\r\n\0]/.test(choice.path))) throw new Error('网站目录必须是服务器上的绝对路径')
  const field = site ? 'siteDir' : fields[type]
  if (!field || !choice.id.startsWith(type + ':')) throw new Error('选择的运行环境与应用类型不匹配')
  if (type !== 'static' && (!choice.path.startsWith('/') || /[\r\n\0]/.test(choice.path))) throw new Error('运行环境必须使用服务器上的绝对路径')
  if (type === 'static' && !site && !/^(?:[a-z][a-z0-9-]{0,30}|system:nginx\.service)$/.test(choice.path)) throw new Error('请选择有效的 Nginx 实例')
  const text=raw.replace(/\r\n?/g,'\n')
  const doc=parse(text)
  if (doc.errors.length) throw new Error('配置文件存在 YAML 语法错误，请先修正')
  const apps=nodeAt(doc,['apps'])
  if (!isSeq(apps)) throw new Error('配置中没有应用')
  const index=apps.items.findIndex((_,index)=>scalarAt(doc,['apps',index,'name'])===app)
  if (index<0 || scalarAt(doc,['apps',index,'type'])!==type) throw new Error('应用配置已变化，请重新读取')
  if (!nodeAt(doc,['targets',target])) throw new Error('服务器配置已不存在')
  const at=['targets',target,'appRuntimes',app]
  if (hasAlias(doc,at)) throw new Error('这里使用了 YAML 别名，请先在配置编辑器中调整运行环境')
  const value: Record<string,string> = { [field]:choice.path }
  if (site) {
    // 已有站点：port 是它的端口（部署后检查首页能打开）；同一个端口上有几个站点时按域名区分，不检查
    const p = choice.site?.ports?.[0]
    if (p && !choice.site?.shared) value.port = String(p)
  } else if (type==='static') {
    if (!/^(?:(?:\d{1,3}(?:\.\d{1,3}){3}|\[[0-9a-fA-F:]+\]|localhost):)?\d{1,5}$/.test(port.trim())) throw new Error('请填写网站端口，例如 8081 或 127.0.0.1:8081')
    const number=Number(port.trim().split(':').at(-1))
    if (number<1 || number>65535) throw new Error('网站端口应在 1～65535 之间')
    value.port=port.trim()
  }
  setValue(doc,at,value)
  const out=serialize(text,doc)
  return raw.includes('\r\n') ? out.replace(/\n/g,'\r\n') : out
}
