// 界面的通用约定。

/** 鼠标悬停多久（毫秒）才显示说明：避免鼠标划过时到处弹出提示 */
export const HOVER_DELAY = 500

/**
 * 生产服务器（production: true）上的部署、回滚、停止、重启：输入服务器名才执行，防止点错。
 * 返回 true = 已确认；取消或关掉返回 false。
 */
export async function confirmProduction(what: string, target: string): Promise<boolean> {
  const { ElMessageBox } = await import('element-plus')
  try {
    await ElMessageBox.prompt(`${target} 是生产服务器，这次要${what}，会影响线上业务。输入服务器名 ${target} 确认：`, '生产服务器', {
      type: 'warning',
      confirmButtonText: '确认执行',
      inputPlaceholder: target,
      inputValidator: (v: string) => v.trim() === target || '服务器名不对',
    })
    return true
  } catch {
    return false
  }
}
