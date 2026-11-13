# 10. 英文 README、快速上手、常见问题

## 现状

- `README.md` 650 多行，从功能列表一路讲到命令表、目录结构；`docs/config.md` 近 600 行。全是中文。
- 界面没有多语言（`desktop/src` 里没有 i18n），所有文字直接写在组件里。
- 没有截图、动图、视频；没有「常见问题」「出错了怎么办」的专页；错误提示本身写得很清楚（这是长处），但用户要先知道去哪找。
- 开源（2026-10-30）之后：国外用户基本看不懂；国内新用户第一次打开 README 要翻很久才知道「我该先干什么」。

## 目标

1. **5 分钟上手**（`docs/quickstart.md`）：从装好 Ferry 到第一次部署成功，只讲最短路径，每步一张截图。
2. **常见问题**（`docs/faq.md`）：按「看到这句提示 → 原因 → 怎么办」组织，覆盖最常见的 20 个错误。
3. **README 变短**：只留「是什么、能做什么（配图）、怎么装、5 分钟上手的链接、文档目录」，长内容挪到 `docs/` 下。
4. **英文版**：`README.en.md`、`docs/en/quickstart.md`、`docs/en/config.md`（配置参考，最有用）。界面多语言放到以后（工作量大，单独立项）。

不做：文档网站（先把内容理顺；以后要建站时这些 Markdown 直接能用，比如 VitePress）。

## 结构

```
README.md                 精简版（中文）
README.en.md              英文
docs/
  quickstart.md           5 分钟上手
  faq.md                  常见问题
  config.md               配置参考（现有）
  ci.md                   在 CI 里用（现有）
  guides/                 从 README 挪出来的专题
    deploy.md             部署：增量上传、就绪、回滚、不停机发布、多台
    servers.md            服务器：端口防火墙、软件、文件、终端、登录用户
    monitor-backup.md     告警、数据备份、异地备份
    team.md               团队协作、部署名单、通知
    ai-mcp.md             AI 助手、MCP
    security.md           安全默认值、凭据存在哪
  en/
    quickstart.md
    config.md
  plans/                  改进计划（本目录）
  images/                 截图（PNG，压缩到每张 < 200 KB）
```

## 常见问题的来源

不靠猜，从代码里的错误提示和用户实际遇到过的问题整理：

- 代码：`rg 'fmt.Errorf\("|a.result\(proto.Code' internal/` 里面向用户的提示，按出现场景分组，挑最常见的。
- 已知的坑（自己的记录里有）：主机公钥变了、私钥在别的电脑上（本机设置）、sudo 慢（/etc/hosts）、运行用户没有权限（自动修正）、nginx 实例冲突、1Panel 容器里的数据库、OpenSSH 会话数上限、服务器进程太多导致查看慢。
- 每条写：提示原文（便于搜索）→ 为什么 → 怎么办（桌面版点哪里 / 命令）。

## 截图

- 用开发界面（`node desktop/scripts/dev-bridge.mjs` + `npm run dev`）配 e2e 容器截，**不用真实服务器**（避免截到真实地址、项目名）；截图里的项目、服务器名统一用示例名（shop、prod、192.0.2.10）。
- 深色、浅色各截关键的几张（首页、部署对话框、应用详情、端口页、告警）。

## 英文

- 先翻 `quickstart` 和 `config.md`（配置参考对外国用户最有用）；`config.md` 和 `schema.ts` 的说明是同一份知识，英文版以后可以从 schema 生成字段表，避免两边不一致。
- 界面里出现的按钮名在英文文档里写中文原文 + 括号英文意思（界面还没有英文版），例如「部署 (Deploy)」。

## 分步

1. `docs/quickstart.md` + 截图（中文）。
2. `docs/faq.md`（中文，先 20 条）。
3. README 精简，长内容挪进 `docs/guides/`（只挪不改，链接全部检查一遍）。
4. 英文 `README.en.md`、`docs/en/quickstart.md`。
5. 英文 `docs/en/config.md`。
6. 之后每做完 `plans/` 里的一项，同时更新中英文文档。

## 验收

- 一个没用过 Ferry 的人只看 `quickstart.md`，能在 10 分钟内把一个 Spring Boot jar 部署到一台新服务器。
- README 首屏能看到：一句话介绍、一张截图、安装链接、上手链接。
- 文档里所有相对链接都有效（加一个脚本 `scripts/checklinks` 在 CI 里跑）。
