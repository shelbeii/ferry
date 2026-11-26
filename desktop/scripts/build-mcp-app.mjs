// 把 desktop/src/mcpapp/confirm.ts（含 @modelcontextprotocol/ext-apps SDK）打包成单文件 HTML，
// 输出到 internal/mcpserver/confirm_app.html，供 go:embed 嵌入到 ferry 二进制中。
import { rolldown } from 'rolldown'
import { writeFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, resolve } from 'node:path'

const here = dirname(fileURLToPath(import.meta.url))
const entry = resolve(here, '../src/mcpapp/confirm.ts')
const outHtml = resolve(here, '../../internal/mcpserver/confirm_app.html')

const bundle = await rolldown({
  input: entry,
  platform: 'browser',
})

const { output } = await bundle.generate({
  format: 'iife',
  name: 'FerryConfirmApp',
  minify: true,
})
await bundle.close()

const js = output[0].code.replace(/<\/script>/gi, '<\\/script>')

const css = `
:root {
  color-scheme: light dark;
}
* { box-sizing: border-box; }
body {
  margin: 0;
  padding: 8px;
  background: var(--color-background-primary, transparent);
  color: var(--color-text-primary, #1f2328);
  font-family: var(--font-sans, -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif);
  font-size: 13px;
  line-height: 1.5;
}
.card {
  border: 1px solid var(--color-border-primary, rgba(128,128,128,0.28));
  border-radius: 10px;
  background: var(--color-background-secondary, rgba(128,128,128,0.06));
  padding: 12px 14px;
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.head {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
}
.title { font-weight: 600; }
.badge {
  display: inline-flex;
  align-items: center;
  padding: 2px 8px;
  border-radius: 6px;
  font-size: 12px;
  font-weight: 600;
  border: 1px solid transparent;
}
.badge.primary {
  background: var(--color-background-info, rgba(59,130,246,0.14));
  color: var(--color-text-info, #2563eb);
  border-color: var(--color-border-info, rgba(59,130,246,0.32));
}
.badge.warning {
  background: var(--color-background-warning, rgba(245,158,11,0.15));
  color: var(--color-text-warning, #b45309);
  border-color: var(--color-border-warning, rgba(245,158,11,0.35));
}
.badge.danger {
  background: var(--color-background-danger, rgba(239,68,68,0.14));
  color: var(--color-text-danger, #dc2626);
  border-color: var(--color-border-danger, rgba(239,68,68,0.35));
}
.badge.danger.solid {
  background: var(--color-text-danger, #dc2626);
  color: var(--color-text-inverse, #fff);
}
.badge.success {
  background: var(--color-background-success, rgba(16,185,129,0.14));
  color: var(--color-text-success, #059669);
  border-color: var(--color-border-success, rgba(16,185,129,0.35));
}
.pill {
  margin-left: auto;
  font-size: 11px;
  padding: 1px 7px;
  border-radius: 999px;
  border: 1px solid var(--color-border-secondary, rgba(128,128,128,0.25));
}
.muted { color: var(--color-text-secondary, #6b7280); }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: 12px; }
.meta {
  display: flex;
  flex-direction: column;
  gap: 4px;
  padding: 8px 10px;
  border-radius: 8px;
  background: var(--color-background-tertiary, rgba(128,128,128,0.06));
  border: 1px solid var(--color-border-secondary, rgba(128,128,128,0.18));
}
.meta-row { display: flex; gap: 10px; align-items: baseline; }
.meta-label { flex: 0 0 68px; font-size: 12px; color: var(--color-text-secondary, #6b7280); }
.meta-val { flex: 1; word-break: break-word; }
.meta-val.reason { font-weight: 600; color: var(--color-text-primary, inherit); }
.meta-val.warn { font-weight: 600; color: var(--color-text-warning, #b45309); }
.meta-val.danger { font-weight: 600; color: var(--color-text-danger, #dc2626); }
.detail-list {
  margin: 0;
  padding-left: 18px;
  display: flex;
  flex-direction: column;
  gap: 2px;
  font-size: 12px;
}
.code-wrap { display: flex; flex-direction: column; gap: 4px; }
.code-head { display: flex; align-items: center; justify-content: space-between; gap: 8px; }
.code-title { font-size: 12px; color: var(--color-text-secondary, #6b7280); }
.link-btn {
  appearance: none;
  border: none;
  background: transparent;
  padding: 0;
  font-size: 11.5px;
  color: var(--color-text-info, #2563eb);
  cursor: pointer;
}
.link-btn:hover { text-decoration: underline; }
.reason-row { display: flex; align-items: center; gap: 6px; }
.reason-input {
  flex: 1;
  min-width: 0;
  border: 1px solid var(--color-border-primary, rgba(128,128,128,0.32));
  border-radius: 6px;
  padding: 5px 8px;
  font-size: 12px;
  background: var(--color-background-primary, transparent);
  color: var(--color-text-primary, inherit);
}
.reason-input:focus { outline: none; border-color: var(--color-text-info, #2563eb); }
.code-pre {
  margin: 0;
  padding: 8px 10px;
  max-height: 240px;
  overflow: auto;
  border-radius: 7px;
  border: 1px solid var(--color-border-secondary, rgba(128,128,128,0.24));
  background: var(--color-background-primary, rgba(0,0,0,0.04));
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 12px;
  line-height: 1.45;
  white-space: pre-wrap;
  word-break: break-all;
}
.code-pre.compact { max-height: 160px; }
.line.add { color: var(--color-text-success, #16a34a); }
.line.del { color: var(--color-text-danger, #dc2626); }
.line.hunk { color: var(--color-text-info, #2563eb); }
.foot {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  padding-top: 4px;
}
.countdown { font-size: 12px; }
.fail-text { color: var(--color-text-danger, #dc2626); }
.actions { display: flex; gap: 8px; }
.btn {
  appearance: none;
  border: 1px solid var(--color-border-primary, rgba(128,128,128,0.3));
  border-radius: 7px;
  padding: 6px 14px;
  font-size: 12.5px;
  font-weight: 600;
  cursor: pointer;
  background: var(--color-background-primary, transparent);
  color: var(--color-text-primary, inherit);
}
.btn:disabled { opacity: 0.55; cursor: not-allowed; }
.btn.primary {
  background: var(--color-text-info, #2563eb);
  border-color: transparent;
  color: var(--color-text-inverse, #fff);
}
.btn.danger {
  background: var(--color-text-danger, #dc2626);
  border-color: transparent;
  color: var(--color-text-inverse, #fff);
}
.status {
  padding: 8px 10px;
  border-radius: 8px;
  border: 1px solid var(--color-border-secondary, rgba(128,128,128,0.24));
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.status.ok {
  border-color: var(--color-border-success, rgba(16,185,129,0.45));
  background: var(--color-background-success, rgba(16,185,129,0.09));
}
.status.running {
  border-color: var(--color-border-info, rgba(59,130,246,0.45));
  background: var(--color-background-info, rgba(59,130,246,0.09));
}
.status.fail {
  border-color: var(--color-border-danger, rgba(239,68,68,0.45));
  background: var(--color-background-danger, rgba(239,68,68,0.09));
}
.btn.sm { padding: 3px 9px; font-size: 11.5px; font-weight: 500; }
.status-head-row { display: flex; align-items: center; justify-content: space-between; gap: 8px; }
.status-head { font-weight: 600; font-size: 12.5px; }
.status-msg { font-size: 12px; white-space: pre-wrap; word-break: break-word; }
`

const html = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width, initial-scale=1" />
<title>Ferry 改动确认</title>
<style>${css.trim()}</style>
</head>
<body>
<div id="app"></div>
<script>${js}</script>
</body>
</html>
`

writeFileSync(outHtml, html, 'utf8')
console.log(`Built ${outHtml} (${html.length} bytes)`)
