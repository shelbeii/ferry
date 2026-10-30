/** 远端文件操作接收一个名字，禁止把空白、当前目录或上级目录当成新名字。 */
export function validRemoteName(value: string) {
  const name = value.trim()
  return !!name && name !== '.' && name !== '..' && !/[/\u0000]/.test(name)
}
