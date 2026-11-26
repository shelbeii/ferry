// 界面画出来之前，按上次的主题切好基座和专属配色。
// 主题在 settings.json 里，要等 ferry 读完设置才知道；那时界面已经按浅色画过一次了，其他主题下会先闪一下浅色。
// 已保存的主题由 state.ts 记在 localStorage 的 ferry.theme（第一次打开时没有：跟随系统）。
;(function () {
  var theme = null
  try {
    theme = localStorage.getItem('ferry.theme')
  } catch (e) {}
  var known = { light: 0, dark: 1, 'warm-paper': 0, 'warm-dusk': 1, 'dimmed-sage': 1, obsidian: 1 }
  var resolved
  if (theme && Object.prototype.hasOwnProperty.call(known, theme)) {
    resolved = theme
  } else {
    resolved = window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'
  }
  var root = document.documentElement
  root.setAttribute('data-theme', resolved)
  if (known[resolved]) root.classList.add('dark')
  var favicon = document.getElementById('ferry-favicon')
  if (favicon) favicon.href = '/branding/ferry-' + resolved + '.png'
})()
