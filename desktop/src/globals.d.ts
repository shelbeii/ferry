// 模板里直接用的全局常量（main.ts 里注册）
export {}

declare module 'vue' {
  interface ComponentCustomProperties {
    /** 悬停多久才显示说明（毫秒） */
    HOVER_DELAY: number
  }
}
