# GitHub 发布说明

发布工作流会在下载说明前加入「本次更新」，默认提取**上一个已发布正式 Release 到当前标签**之间的提交标题。没有发布 Release 的中间标签、草稿和预发布不会截断更新范围；重新运行旧版本也不会混入后续版本的提交。

提交标题中的 `0.13.54：` 这类版本前缀会自动去掉，重复标题会合并。直接提交到 main 的功能调整和修复也会列出，无需先创建 PR。

需要像产品公告那样整理内容时，在打标签前提交 `docs/releases/v<版本号>.md`。例如 `v0.13.54.md`：

```markdown
- 新增内置浏览器，可在 Ferry 中直接检查部署后的页面。
- 开发者工具停靠在网页内，减少窗口切换和资源占用。
```

该文件只写更新内容，不需要写「本次更新」标题和下载表格。发布时读取标签所指提交中的文件；文件为空会停止发布。已有 Release 的工作流重新运行时，也会同步更新发布说明。

本地预览一个已打标签的版本（不发布、不推送）：

```powershell
node scripts/release-notes.mjs --tag v0.13.54 --previous-tag v0.13.39 --repo shelbeii/ferry
```

检查生成逻辑：

```powershell
node --test scripts/release-notes.test.mjs
```
