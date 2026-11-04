# 在 GitHub 上发布现在的版本：检查后给当前提交打上 v<版本号> 标签并推送，
# GitHub 上的「发布」工作流（.github/workflows/release.yml）随后构建 Windows、macOS、Linux 的安装包，建好 Release。
#   双击 scripts\release.cmd
#   powershell -File scripts\release.ps1            # 和双击一样，发布前会问一次
#   powershell -File scripts\release.ps1 -DryRun    # 只检查、列出要发布的内容，不打标签
#   powershell -File scripts\release.ps1 -Yes       # 不问，直接发布
#   powershell -File scripts\release.ps1 -Yes -NoOpen  # 自动发布，不打开浏览器
#
# 版本号取自 desktop/package.json；要发新版本先升版本号（scripts\package.ps1 -Bump）并提交、推送。
# 发布前要求：在 main 分支、没有没提交的修改、本地和 GitHub 上的 main 一致（本地多出的提交会问要不要先推送）、这个版本还没发布过。
param(
    [switch]$DryRun,
    [switch]$Yes,
    [switch]$NoOpen
)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

function Step($text) { Write-Host "`n==> $text" -ForegroundColor Cyan }
function Fail($text) { Write-Host "`n✗ $text" -ForegroundColor Red; exit 1 }

# 每个 git 查询都检查退出码，避免把网络失败误认为标签不存在。
# Windows PowerShell 5.1 会把原生命令的 stderr 当作错误记录，先收集再按退出码判断。
function Invoke-Git([string[]]$GitArgs) {
    $savedPreference = $ErrorActionPreference
    $savedEncoding = [Console]::OutputEncoding
    $ErrorActionPreference = 'Continue'
    try {
        # PowerShell 5.1 按 Console.OutputEncoding 解码外部程序；与 Git 的 UTF-8 输出保持一致。
        [Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false)
        $output = @(& git -c i18n.logOutputEncoding=utf-8 @GitArgs 2>&1)
        $code = $LASTEXITCODE
    } finally {
        [Console]::OutputEncoding = $savedEncoding
        $ErrorActionPreference = $savedPreference
    }
    if ($code -ne 0) { throw "git $($GitArgs -join ' ') 失败（退出码 $code）：`n$($output -join "`n")" }
    $output | ForEach-Object { $_.ToString() }
}

# 连 GitHub 偶尔失败（网络）：最多试 3 次
function Git-Retry([string]$what) {
    for ($i = 1; $i -le 3; $i++) {
        try {
            Invoke-Git -GitArgs $args | ForEach-Object { Write-Host $_ }
            return
        } catch {
            Write-Host $_.Exception.Message -ForegroundColor Yellow
        }
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
if ($version -notmatch '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$') { Fail "正式版版本号应为 x.y.z：$version" }
# 安装包名称取自 Tauri，Rust 和 npm 也各有版本记录，发布前必须全部一致。
$lockText = [IO.File]::ReadAllText((Join-Path $root 'desktop\package-lock.json'))
# Windows PowerShell 5.1 的 ConvertFrom-Json 不支持 packages[""]，只在内存里临时改名。
$lockText = ([regex]'"packages"\s*:\s*\{\s*""\s*:').Replace($lockText, '"packages":{"__ferry_root__":', 1)
$lock = $lockText | ConvertFrom-Json
$conf = [IO.File]::ReadAllText((Join-Path $root 'desktop\src-tauri\tauri.conf.json')) | ConvertFrom-Json
$cargo = [IO.File]::ReadAllText((Join-Path $root 'desktop\src-tauri\Cargo.toml'))
$cargoLock = [IO.File]::ReadAllText((Join-Path $root 'desktop\src-tauri\Cargo.lock'))
$versions = [ordered]@{
    'package-lock.json' = $lock.version
    'package-lock.json packages[""]' = $lock.packages.__ferry_root__.version
    'tauri.conf.json' = $conf.version
    'Cargo.toml' = ([regex]'(?ms)^\[package\]\s*.*?^version\s*=\s*"([^"]+)"').Match($cargo).Groups[1].Value
    'Cargo.lock' = ([regex]'(?m)^name = "ferry-desktop"\r?\nversion = "([^"]+)"').Match($cargoLock).Groups[1].Value
}
foreach ($name in $versions.Keys) {
    if ($versions[$name] -ne $version) { Fail "$name 的版本号（$($versions[$name])）与 package.json（$version）不一致：请先用 package.ps1 同步版本号并提交" }
}
$tag = "v$version"
$remote = (Invoke-Git -GitArgs @('remote', 'get-url', 'origin')).Trim()
$repo = ([regex]'github\.com[:/](.+?)(\.git)?$').Match($remote).Groups[1].Value
if (-not $repo) { Fail "origin 不是 GitHub 仓库：$remote" }
Step "要发布 Ferry $version（标签 $tag，仓库 $repo）"

# ---- 分支、本地修改
$branch = (Invoke-Git -GitArgs @('rev-parse', '--abbrev-ref', 'HEAD')).Trim()
if ($branch -ne 'main') { Fail "现在在 $branch 分支：请切到 main 再发布" }
$dirty = @(Invoke-Git -GitArgs @('status', '--porcelain', '--untracked-files=no'))
if ($dirty) { Fail "有还没提交的修改，先提交（发布的是已提交的代码）：`n$($dirty -join "`n")" }
$untracked = @(Invoke-Git -GitArgs @('ls-files', '--others', '--exclude-standard') | Where-Object { $_ -notlike '.claude/*' })
if ($untracked) { Fail "有尚未加入提交的文件：`n$($untracked -join "`n")`n请提交需要发布的文件，或将本地文件加入忽略规则" }

# ---- 和 GitHub 上的比较
Step '读取 GitHub 上的分支和标签'
Git-Retry '读取 GitHub' fetch origin main --tags --quiet
$counts = (Invoke-Git -GitArgs @('rev-list', '--left-right', '--count', 'origin/main...HEAD')).Trim() -split '\s+'
$behind, $ahead = [int]$counts[0], [int]$counts[1]
if ($behind -gt 0) { Fail "GitHub 上的 main 有 $behind 个本地没有的提交：先 git pull 合并，再发布" }

$remoteTag = Invoke-Git -GitArgs @('ls-remote', '--tags', 'origin', "refs/tags/$tag")
if ($remoteTag) {
    Write-Host "`nGitHub 上已有标签 $tag，不再重复推送。请到 Actions 查看构建结果；构建失败时重新运行该工作流。" -ForegroundColor Yellow
    Write-Host "构建页面：https://github.com/$repo/actions/workflows/release.yml"
    Write-Host "Release 页面：https://github.com/$repo/releases/tag/$tag"
    exit 0
}
$localTag = Invoke-Git -GitArgs @('tag', '--list', $tag)
if ($localTag) {
    $tagCommit = (Invoke-Git -GitArgs @('rev-list', '-n', '1', $tag)).Trim()
    $head = (Invoke-Git -GitArgs @('rev-parse', 'HEAD')).Trim()
    if ($tagCommit -ne $head) { Fail "本地已有标签 $tag，但不在当前提交上：确认后用 git tag -d $tag 删掉再运行" }
}

# ---- 要发布的内容
# 没有更早的标签时 git describe 会报错：PowerShell 5.1 里重定向错误输出也会中止脚本，这里先放宽
$ErrorActionPreference = 'Continue'
$prev = (& git describe --tags --abbrev=0 --match 'v*' --exclude=$tag HEAD 2>$null)
$ErrorActionPreference = 'Stop'
$log = if ($prev) { @(Invoke-Git -GitArgs @('log', '--oneline', "$prev..HEAD")) } else { @(Invoke-Git -GitArgs @('log', '--oneline', '-20')) }
if ($prev -and -not $log) { Fail "当前提交就是上次发布的 $prev：没有新的提交，不用再发布" }
Write-Host ''
Write-Host ("提交：{0}" -f (Invoke-Git -GitArgs @('log', '-1', '--format=%h %s')))
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
    Invoke-Git -GitArgs @('tag', '-a', $tag, '-m', "Ferry $version")
}
Step "推送标签 $tag"
Git-Retry '推送标签' push --quiet origin $tag

$actions = "https://github.com/$repo/actions/workflows/release.yml"
Write-Host "`n已推送 $tag。GitHub 将构建各平台安装包，全部成功后建立 Release。" -ForegroundColor Green
Write-Host "进度：$actions"
Write-Host "做完后在：https://github.com/$repo/releases/tag/$tag"
if (-not $NoOpen) { Start-Process $actions }
