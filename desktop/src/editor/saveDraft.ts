/** 保存前检查磁盘内容；返回真正写入的快照，等待期间的新输入仍应保持未保存。 */
export async function saveDraft(
  baseline: string,
  draft: string,
  io: { read: () => Promise<string>; write: (content: string) => Promise<void> },
) {
  const current = await io.read()
  if (current.replace(/\r\n?/g, '\n') !== baseline.replace(/\r\n?/g, '\n')) {
    throw new Error('配置文件已被其他操作修改，本次未保存。请先备份当前草稿，再重新打开配置合并修改。')
  }
  await io.write(draft)
  return draft.replace(/\r\n?/g, '\n')
}
