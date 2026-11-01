import type { InjectionKey, Ref } from 'vue'

/** 抽屉滑动期间暂停新折射贴图的计算，保留已有材质；嵌套开关等控件一起遵守。 */
export const glassMotionKey: InjectionKey<Readonly<Ref<boolean>>> = Symbol('glass-motion')
