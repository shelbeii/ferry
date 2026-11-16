# 在 GitHub 上发布当前 main。已有发布之后新增了提交时，会在确认后同步递增版本号。
# GitHub 上的「发布」工作流（.github/workflows/release.yml）随后构建 Windows、macOS、Linux 的安装包，建好 Release。
#   双击 scripts\release.cmd
#   powershell -File scripts\release.ps1            # 和双击一样，发布前会问一次
#   powershell -File scripts\release.ps1 -DryRun    # 预览版本与提交，不写版本文件、不推送
#   powershell -File scripts\release.ps1 -Yes       # 不问，直接发布
#   powershell -File scripts\release.ps1 -Yes -NoOpen  # 自动发布，不打开浏览器
#
# 发布前要求在 main 分支、工作区干净、本地包含远程 main 的全部提交。
param(
    [switch]$DryRun,
    [switch]$Yes,
    [switch]$NoOpen
)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root
. (Join-Path $PSScriptRoot 'desktop-version.ps1')

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
$versionFiles = @(Read-DesktopVersionFiles $root)
$current = $versionFiles[0].Versions[0]
$versionPattern = '(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)'
if ($current -notmatch "^$versionPattern$") { Fail "正式版版本号应为 x.y.z：$current" }
foreach ($entry in $versionFiles) {
    foreach ($recorded in $entry.Versions) {
        if ($recorded -ne $current) { Fail "$($entry.File) 的版本号（$recorded）与 package.json（$current）不一致：请先用 package.ps1 同步版本号并提交" }
    }
}
$remote = (Invoke-Git -GitArgs @('remote', 'get-url', 'origin')).Trim()
$repo = ([regex]'github\.com[:/](.+?)(\.git)?$').Match($remote).Groups[1].Value
if (-not $repo) { Fail "origin 不是 GitHub 仓库：$remote" }

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

$head = (Invoke-Git -GitArgs @('rev-parse', 'HEAD')).Trim()
$releaseTags = @{}
foreach ($line in @(Invoke-Git -GitArgs @('ls-remote', '--tags', 'origin', 'refs/tags/v*'))) {
    if ($line -match "^(?<oid>[0-9a-f]{40,64})\s+refs/tags/(?<tag>v$versionPattern)(?<peeled>\^\{\})?$") {
        # 轻量标签直接指向提交，附注标签使用剥离后的提交。
        if ($Matches['peeled'] -or -not $releaseTags.ContainsKey($Matches['tag'])) {
            $releaseTags[$Matches['tag']] = $Matches['oid']
        }
    }
}
$tag = "v$current"
if ($releaseTags[$tag] -eq $head) {
    Write-Host "`nGitHub 上已有标签 $tag，不再重复推送。请到 Actions 查看构建结果；构建失败时重新运行该工作流。" -ForegroundColor Yellow
    Write-Host "构建页面：https://github.com/$repo/actions/workflows/release.yml"
    Write-Host "Release 页面：https://github.com/$repo/releases/tag/$tag"
    exit 0
}
$version = $current
$latest = $releaseTags.Keys | Sort-Object { [version]($_.Substring(1)) } -Descending | Select-Object -First 1
if ($latest -and [version]$current -le [version]($latest.Substring(1))) {
    $latestHead = $releaseTags[$latest]
    if ($latestHead -eq $head) { Fail "当前提交已有发布标签 $latest，但文件里的版本号是 $current：请检查版本号" }
    $outside = (Invoke-Git -GitArgs @('rev-list', '--count', "$head..$latestHead")).Trim()
    if ($outside -ne '0') { Fail "当前 main 未包含已发布的 $latest：请先同步发布历史" }
    $version = Next-DesktopVersion ($latest.Substring(1))
    $tag = "v$version"
}
$bump = $version -ne $current
Step "要发布 Ferry $version（标签 $tag，仓库 $repo）"
if ($bump) { Write-Host "版本号将从 $current 自动更新到 $version，并提交到 main。" }
$localTag = Invoke-Git -GitArgs @('tag', '--list', $tag)
if ($localTag) {
    $tagCommit = (Invoke-Git -GitArgs @('rev-list', '-n', '1', $tag)).Trim()
    if ($bump -or $tagCommit -ne $head) { Fail "本地标签 $tag 指向另一份发布内容：请先检查该标签" }
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
if ($ahead -gt 0) { Write-Host "`n本地有 $ahead 个提交还没推送到 GitHub，发布时会将 main 和标签一起推送。" -ForegroundColor Yellow }

if ($DryRun) {
    Write-Host "`n（只检查：-DryRun，未修改版本文件、未提交、未推送）" -ForegroundColor Green
    exit 0
}
if (-not (Ask "`n发布 Ferry $version 到 GitHub？")) {
    Write-Host '已取消'
    exit 0
}
if ((Invoke-Git -GitArgs @('rev-parse', '--abbrev-ref', 'HEAD')).Trim() -ne $branch -or
    (Invoke-Git -GitArgs @('rev-parse', 'HEAD')).Trim() -ne $head) {
    Fail '确认前后当前分支或提交发生了变化，请检查后重试'
}

# ---- 提交版本号、打标签、推送
if ($bump) {
    Step "提交版本号 $version"
    try {
        Write-DesktopVersions $versionFiles $version
        foreach ($entry in @(Read-DesktopVersionFiles $root)) {
            if ($entry.Versions | Where-Object { $_ -ne $version }) { throw "$($entry.File) 的版本号没有同步到 $version" }
        }
        $paths = @($versionFiles | ForEach-Object { $_.File })
        Invoke-Git -GitArgs (@('commit', '--only', '-m', "发布 Ferry $version", '--') + $paths)
    } catch {
        $reason = $_.Exception.Message
        if ((Invoke-Git -GitArgs @('rev-parse', 'HEAD')).Trim() -eq $head) {
            Write-DesktopVersions $versionFiles $version -Restore
        }
        Fail "无法提交版本号 $version：$reason"
    }
    $head = (Invoke-Git -GitArgs @('rev-parse', 'HEAD')).Trim()
}
if (-not $localTag) {
    Step "打标签 $tag"
    Invoke-Git -GitArgs @('tag', '-a', $tag, $head, '-m', "Ferry $version")
}
Step "推送 main 和标签 $tag"
Git-Retry '推送发布' push --atomic --no-follow-tags --quiet origin "${head}:refs/heads/main" "refs/tags/${tag}:refs/tags/$tag"

$actions = "https://github.com/$repo/actions/workflows/release.yml"
Write-Host "`n已推送 $tag。GitHub 将构建各平台安装包，全部成功后建立 Release。" -ForegroundColor Green
Write-Host "进度：$actions"
Write-Host "做完后在：https://github.com/$repo/releases/tag/$tag"
if (-not $NoOpen) { Start-Process $actions }
