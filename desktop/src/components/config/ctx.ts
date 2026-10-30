// 表单编辑配置时各层组件共用的东西（ConfigForm 提供）。
import type { ComputedRef, InjectionKey, Ref } from 'vue'
import type { Document } from 'yaml'

export interface FormCtx {
  /** 当前配置（每次修改后重新解析） */
  doc: ComputedRef<Document>
  /** 修改配置：在一份新解析的文档上改，再写回文字 */
  edit: (fn: (d: Document) => void) => void
  /** 配置里的构建名、服务器名、应用名（下拉候选） */
  names: ComputedRef<{ builds: string[]; targets: string[]; apps: string[] }>
  /** 只显示写了的配置 */
  onlySet: Ref<boolean>
}

export const formCtx: InjectionKey<FormCtx> = Symbol('formCtx')
