import type { InspectTarget, Software } from './ferry'

export interface FileLocation {
  label: string
  path: string
}

export function softwareLocations(row: Software): FileLocation[] {
  if (row.dir?.startsWith('/')) return [{ label: '安装目录', path: row.dir }]
  // 容器内的程序路径不属于主机文件系统，只浏览已识别的主机位置。
  if (row.via === 'docker') return row.locations ?? []
  return row.path?.startsWith('/') ? [{ label: '程序位置', path: row.path }] : []
}

export function appLocation(target: Pick<InspectTarget, 'root' | 'appRoots'> | undefined, name: string): string {
  const root = target?.appRoots?.[name] || target?.root
  return root?.startsWith('/') ? `${root.replace(/\/+$/, '')}/apps/${name}` : ''
}
