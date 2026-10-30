# 打 Windows 安装包（NSIS）：可选升版本号，先做检查（go vet、gofmt、go test、界面类型检查），再构建，最后列出安装包。
#   powershell -File scripts\package.ps1                  # 用现在的版本号打包
#   powershell -File scripts\package.ps1 -Bump            # 版本号末位加 1 再打包（0.12.48 -> 0.12.49）
#   powershell -File scripts\package.ps1 -Version 0.13.0  # 指定版本号再打包
#   powershell -File scripts\package.ps1 -Bump -SkipCheck # 跳过检查，直接打包（比如刚检查过）
#   powershell -File scripts\package.ps1 -Open            # 打好后在资源管理器里选中安装包
#   powershell -File scripts\package.ps1 -Ask             # 先问要不要把版本号末位加 1（双击 scripts\package.cmd 就是这样）
#
# 版本号写在四个文件里：desktop/package.json、desktop/package-lock.json、desktop/src-tauri/tauri.conf.json、
# desktop/src-tauri/Cargo.toml（Cargo.lock 里的也一起改），脚本会一起改。只改文件，不提交。
#
# Go、Rust、Node 不在 PATH 里时，依次到 -GoDir、-CargoDir、-NodeDir 找（默认是这台机器上的安装位置）。
param(
    [string]$Version = '',
    [switch]$Bump,
    [switch]$SkipCheck,
    [switch]$Open,
    [switch]$Ask,
    [string]$GoDir = 'E:\go\install\bin',
    [string]$CargoDir = 'E:\rust\cargo\bin',
    [string]$NodeDir = 'E:\node\install'
)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$desktop = Join-Path $root 'desktop'
$utf8 = New-Object System.Text.UTF8Encoding($false)

function Step($text) { Write-Host "`n==> $text" -ForegroundColor Cyan }

# ---- 工具：找不到就加上默认目录
function Use-Tool([string]$cmd, [string]$dir) {
    if (Get-Command $cmd -ErrorAction SilentlyContinue) { return }
    if ($dir -and (Test-Path (Join-Path $dir "$cmd.exe"))) {
        $env:PATH = "$dir;$env:PATH"
        return
    }
    throw "找不到 $cmd：请把它加进 PATH，或用参数指定所在目录（-GoDir / -CargoDir / -NodeDir）"
}
Use-Tool 'go' $GoDir
Use-Tool 'cargo' $CargoDir
Use-Tool 'node' $NodeDir
# Rust 装在 <目录>\cargo\bin、<目录>\rustup 这种结构下（如 E:\rust）而没设置环境变量时补上
$rustHome = Split-Path -Parent (Split-Path -Parent (Split-Path -Parent (Get-Command cargo).Source))
if (-not $env:CARGO_HOME -and (Test-Path (Join-Path $rustHome 'cargo'))) { $env:CARGO_HOME = Join-Path $rustHome 'cargo' }
if (-not $env:RUSTUP_HOME -and (Test-Path (Join-Path $rustHome 'rustup'))) { $env:RUSTUP_HOME = Join-Path $rustHome 'rustup' }

# ---- 版本号
function Read-Text([string]$path) { [IO.File]::ReadAllText($path) }
function Write-Text([string]$path, [string]$text) { [IO.File]::WriteAllText($path, $text, $utf8) }

$confPath = Join-Path $desktop 'src-tauri\tauri.conf.json'
$current = ([regex]'"version":\s*"([^"]+)"').Match((Read-Text $confPath)).Groups[1].Value
if (-not $current) { throw "读不到 $confPath 里的版本号" }

if ($Bump -and $Version) { throw '-Bump 和 -Version 只能用一个' }
if ($Ask -and -not $Bump -and -not $Version) {
    $parts = $current.Split('.')
    $parts[$parts.Length - 1] = [string]([int]$parts[$parts.Length - 1] + 1)
    $next = $parts -join '.'
    Write-Host "`n现在的版本号是 $current。" -ForegroundColor Cyan
    $a = (Read-Host "升到 $next 再打包？（输入 y 回车 = 升到 $next；也可以直接输入版本号，如 0.13.0；直接回车 = 不改）").Trim()
    if ($a.ToLower() -eq 'y') { $Bump = $true }
    elseif ($a) { $Version = $a }
}
if ($Bump) {
    $parts = $current.Split('.')
    $parts[$parts.Length - 1] = [string]([int]$parts[$parts.Length - 1] + 1)
    $Version = $parts -join '.'
}
if ($Version -and $Version -notmatch '^\d+\.\d+\.\d+$') { throw "版本号格式不对：$Version（应为 x.y.z）" }

# ---- 检查：默认都做，有一项没过就停下，版本号也不改（-SkipCheck 跳过）
if ($SkipCheck) {
    Step '跳过检查（-SkipCheck）'
} else {
    Push-Location $root
    try {
        Step 'go vet ./...'
        & go vet ./...
        if ($LASTEXITCODE) { throw 'go vet 没通过' }
        Step 'gofmt -l internal cmd'
        $bad = & gofmt -l internal cmd
        if ($bad) { throw "这些文件没有 gofmt：`n$($bad -join "`n")" }
        Step 'go test ./...'
        & go test ./...
        if ($LASTEXITCODE) { throw 'go test 没通过' }
    } finally { Pop-Location }
    Push-Location $desktop
    try {
        Step '界面类型检查（vue-tsc）'
        & npx vue-tsc --noEmit -p .
        if ($LASTEXITCODE) { throw '类型检查没通过' }
    } finally { Pop-Location }
}

if ($Version -and $Version -ne $current) {
    Step "版本号 $current -> $Version"
    $old = [regex]::Escape($current)
    # package.json、tauri.conf.json：顶层的 "version"
    foreach ($f in 'package.json', 'src-tauri\tauri.conf.json') {
        $p = Join-Path $desktop $f
        $t = ([regex]"`"version`":\s*`"$old`"").Replace((Read-Text $p), "`"version`": `"$Version`"", 1)
        Write-Text $p $t
    }
    # package-lock.json：开头两处（项目本身和 packages[""]）
    $p = Join-Path $desktop 'package-lock.json'
    Write-Text $p (([regex]"`"version`":\s*`"$old`"").Replace((Read-Text $p), "`"version`": `"$Version`"", 2))
    # Cargo.toml：[package] 里的 version
    $p = Join-Path $desktop 'src-tauri\Cargo.toml'
    Write-Text $p (([regex]"(?m)^version = `"$old`"").Replace((Read-Text $p), "version = `"$Version`"", 1))
    # Cargo.lock：ferry-desktop 那一项
    $p = Join-Path $desktop 'src-tauri\Cargo.lock'
    if (Test-Path $p) {
        Write-Text $p (([regex]"(name = `"ferry-desktop`"\r?\nversion = `")$old`"").Replace((Read-Text $p), "`${1}$Version`"", 1))
    }
    $current = $Version
} else {
    Step "版本号 $current（不改）"
}

# ---- 构建安装包（会先构建内置的 ferry 和 agent，再构建界面和 Rust，最后用 NSIS 打包）
Step '构建安装包（几分钟）'
$started = Get-Date
Push-Location $desktop
try {
    & npx tauri build
    if ($LASTEXITCODE) { throw '构建失败，看上面的输出' }
} finally { Pop-Location }

$setup = Join-Path $desktop "src-tauri\target\release\bundle\nsis\Ferry_${current}_x64-setup.exe"
if (-not (Test-Path $setup)) { throw "构建完成，但没找到安装包：$setup" }
$size = (Get-Item $setup).Length / 1MB
$mins = ((Get-Date) - $started).TotalMinutes
Write-Host ("`n安装包  {0}`n大小    {1:N1} MB，用时 {2:N1} 分钟" -f $setup, $size, $mins) -ForegroundColor Green
if ($Open) { & explorer.exe "/select,`"$setup`"" }
