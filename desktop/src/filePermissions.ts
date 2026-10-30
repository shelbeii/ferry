import type { FileEntry } from './ferry'

interface PermissionRole { role: string; text: string }
export interface FilePermissionInfo {
  summary: PermissionRole[]
  roles: PermissionRole[]
  octal?: string
  special: string[]
  note?: string
}

/** 服务端使用 Go FileMode.String()：类型和特殊位在前，最后九位是 rwxrwxrwx。 */
export function filePermissions(entry: Pick<FileEntry, 'mode' | 'dir' | 'link'>): FilePermissionInfo {
  const mode = entry.mode || ''
  const prefix = mode.slice(0, -9)
  if (entry.link || prefix.includes('L')) {
    return { summary: [{ role: '符号链接', text: '以目标权限为准' }], roles: [], special: [], note: '访问链接时，使用它指向的文件或文件夹的权限。这里的原始权限属于链接本身。' }
  }
  const bits = mode.slice(-9)
  if (!/^[-dalTLDpSugct?]+$/.test(prefix) || !/^(?:[r-][w-][x-]){3}$/.test(bits)) {
    return { summary: [{ role: '', text: '权限信息未知' }], roles: [], special: [], note: '暂时无法解释此权限格式，可查看下方的原始信息。' }
  }
  const digits = [0, 3, 6].map((i) => (bits[i] === 'r' ? 4 : 0) + (bits[i + 1] === 'w' ? 2 : 0) + (bits[i + 2] === 'x' ? 1 : 0))
  const action = entry.dir ? '进入' : '执行'
  const texts = ['无权限', `仅${action}`, '仅写入', `写入、${action}`, '只读', `读取、${action}`, '读写', `读写、${action}`]
  const roles = ['所有者', '同组用户', '其他人'].map((role, i) => ({ role, text: texts[digits[i]] }))
  const summary = digits.every((n) => n === digits[0])
    ? [{ role: '所有人', text: texts[digits[0]] }]
    : digits[1] === digits[2]
      ? [roles[0], { role: '同组及其他人', text: texts[digits[1]] }]
      : roles
  const special: string[] = []
  let specialDigit = 0
  if (prefix.includes('u')) { specialDigit += 4; special.push('设置了 setuid：执行文件时使用文件所有者身份。') }
  if (prefix.includes('g')) { specialDigit += 2; special.push(entry.dir ? '设置了 setgid：新建条目继承此文件夹的所属组。' : '设置了 setgid：执行文件时使用文件所属组身份。') }
  if (prefix.includes('t')) { specialDigit += 1; special.push('设置了粘滞位：通常只有条目所有者、目录所有者或管理员可以删除、改名。') }
  return { summary, roles, octal: `${specialDigit || ''}${digits.join('')}`, special }
}
