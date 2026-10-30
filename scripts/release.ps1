# 在 GitHub 上发布现在的版本：检查后给当前提交打上 v<版本号> 标签并推送，
# GitHub 上的「发布」工作流（.github/workflows/release.yml）随后构建 Windows、macOS、Linux 的安装包，建好 Release。
#   双击 scripts\release.cmd
#   powershell -File scripts\release.ps1            # 和双击一样，发布前会问一次
#   powershell -File scripts\release.ps1 -DryRun    # 只检查、列出要发布的内容，不打标签
#   powershell -File scripts\release.ps1 -Yes       # 不问，直接发布
#
# 版本号取自 desktop/package.json；要发新版本先升版本号（scripts\package.ps1 -Bump）并提交、推送。
# 发布前要求：在 main 分支、没有没提交的修改、本地和 GitHub 上的 main 一致（本地多出的提交会问要不要先推送）、这个版本还没发布过。
param(
    [switch]$DryRun,
    [switch]$Yes
)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

function Step($text) { Write-Host "`n==> $text" -ForegroundColor Cyan }
function Fail($text) { Write-Host "`n✗ $text" -ForegroundColor Red; exit 1 }

# git 的输出原样显示；失败时返回 $false
function Git-Ok {
    & git @args
    return $LASTEXITCODE -eq 0
}

# 连 GitHub 偶尔失败（网络）：最多试 3 次
function Git-Retry([string]$what) {
    for ($i = 1; $i -le 3; $i++) {
        & git @args
        if ($LASTEXITCODE -eq 0) { return }
        if ($i -lt 3) {
            Write-Host "  $what 失败，5 秒后重试（第 $($i + 1) 次）" -ForegroundColor Yellow
            Start-Sleep -Seconds 5
        }
    }
    Fail "$what 失败：检查网络、代理，能打开 github.com 后再试"
}

function Ask([string]$question) {
    if ($Yes) { return $true }
    $a = Read-Host "$question（输入 y 回车确认，直接回车取消）"
    return $a.Trim().ToLower() -eq 'y'
}

if (-not (Get-Command git -ErrorAction SilentlyContinue)) { Fail '找不到 git：请先安装 Git，并加进 PATH' }

# ---- 版本号和仓库
$pkg = Join-Path $root 'desktop\package.json'
$version = ([regex]'"version":\s*"([^"]+)"').Match([IO.File]::ReadAllText($pkg)).Groups[1].Value
if (-not $version) { Fail "读不到 $pkg 里的版本号" }
$tag = "v$version"
$remote = (& git remote get-url origin).Trim()
$repo = ([regex]'github\.com[:/](.+?)(\.git)?$').Match($remote).Groups[1].Value
if (-not $repo) { Fail "origin 不是 GitHub 仓库：$remote" }
Step "要发布 Ferry $version（标签 $tag，仓库 $repo）"

# ---- 分支、本地修改
$branch = (& git rev-parse --abbrev-ref HEAD).Trim()
if ($branch -ne 'main') { Fail "现在在 $branch 分支：请切到 main 再发布" }
$dirty = & git status --porcelain --untracked-files=no
if ($dirty) { Fail "有还没提交的修改，先提交（发布的是已提交的代码）：`n$($dirty -join "`n")" }

# ---- 和 GitHub 上的比较
Step '读取 GitHub 上的分支和标签'
Git-Retry '读取 GitHub' fetch origin main --tags --quiet
$counts = (& git rev-list --left-right --count 'origin/main...HEAD').Trim() -split '\s+'
$behind, $ahead = [int]$counts[0], [int]$counts[1]
if ($behind -gt 0) { Fail "GitHub 上的 main 有 $behind 个本地没有的提交：先 git pull 合并，再发布" }

$remoteTag = & git ls-remote --tags origin "refs/tags/$tag"
if ($remoteTag) {
    Write-Host "`n$tag 已经发布过了（GitHub 上已有这个标签）。要发新版本：先升版本号（scripts\package.ps1 -Bump），提交、推送后再运行。" -ForegroundColor Yellow
    Write-Host "Release 页面：https://github.com/$repo/releases/tag/$tag"
    exit 0
}
$localTag = & git tag --list $tag
if ($localTag) {
    $tagCommit = (& git rev-list -n 1 $tag).Trim()
    $head = (& git rev-parse HEAD).Trim()
    if ($tagCommit -ne $head) { Fail "本地已有标签 $tag，但不在当前提交上：确认后用 git tag -d $tag 删掉再运行" }
}

# ---- 要发布的内容
# 没有更早的标签时 git describe 会报错：PowerShell 5.1 里重定向错误输出也会中止脚本，这里先放宽
$ErrorActionPreference = 'Continue'
$prev = (& git describe --tags --abbrev=0 --match 'v*' HEAD 2>$null)
$ErrorActionPreference = 'Stop'
$log = if ($prev) { & git log --oneline "$prev..HEAD" } else { & git log --oneline -20 }
if ($prev -and -not $log) { Fail "当前提交就是上次发布的 $prev：没有新的提交，不用再发布" }
Write-Host ''
Write-Host ("提交：{0}" -f (& git log -1 --format='%h %s'))
if ($prev) { Write-Host "上次发布：$prev，之后有 $(@($log).Count) 个提交：" } else { Write-Host '最近的提交：' }
$log | Select-Object -First 30 | ForEach-Object { Write-Host "  $_" }
if (@($log).Count -gt 30) { Write-Host "  ……" }
if ($ahead -gt 0) { Write-Host "`n本地有 $ahead 个提交还没推送到 GitHub，发布时会先推送 main。" -ForegroundColor Yellow }

if ($DryRun) {
    Write-Host "`n（只检查：-DryRun，没有打标签、没有推送）" -ForegroundColor Green
    exit 0
}
if (-not (Ask "`n发布 Ferry $version 到 GitHub？")) {
    Write-Host '已取消'
    exit 0
}

# ---- 推送 main、打标签、推送标签
if ($ahead -gt 0) {
    Step '推送 main'
    Git-Retry '推送 main' push --quiet origin main
}
if (-not $localTag) {
    Step "打标签 $tag"
    if (-not (Git-Ok tag -a $tag -m "Ferry $version")) { Fail '打标签失败' }
}
Step "推送标签 $tag"
Git-Retry '推送标签' push --quiet origin $tag

$actions = "https://github.com/$repo/actions/workflows/release.yml"
Write-Host "`n已推送 $tag。GitHub 正在构建、发布（20～30 分钟，macOS 最慢）。" -ForegroundColor Green
Write-Host "进度：$actions"
Write-Host "做完后在：https://github.com/$repo/releases/tag/$tag"
Start-Process $actions
