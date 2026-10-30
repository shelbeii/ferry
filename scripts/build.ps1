# Build ferry: cross-compile ferry-agent for linux/amd64 + linux/arm64 (embedded), then the ferry CLI.
#   powershell -File scripts\build.ps1            # CLI for this machine -> dist\
#   powershell -File scripts\build.ps1 -All       # CLI for windows/linux/darwin
param([string]$Version = '', [switch]$All)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root
if (-not $Version) {
    $ErrorActionPreference = 'Continue'
    $Version = (& git describe --tags --always --dirty 2>$null | Out-String).Trim()
    $ErrorActionPreference = 'Stop'
    if (-not $Version -or $LASTEXITCODE) { $Version = '0.1.0-dev' }
}
$saved = @{ GOOS = $env:GOOS; GOARCH = $env:GOARCH; CGO_ENABLED = $env:CGO_ENABLED }
try {
    $env:CGO_ENABLED = '0'
    $bin = Join-Path $root 'internal\agentbin\bin'
    foreach ($arch in 'amd64', 'arm64') {
        $env:GOOS = 'linux'; $env:GOARCH = $arch
        $out = Join-Path $bin "ferry-agent-linux-$arch"
        # 不写入版本号：agent 代码没变时内容不变，升级后不用重新上传到服务器
        & go build -trimpath -buildvcs=false -ldflags "-s -w" -o $out ./cmd/ferry-agent
        if ($LASTEXITCODE) { throw "build ferry-agent linux/$arch failed" }
        Write-Host ("agent  linux/{0,-6} {1,8:N1} MB" -f $arch, ((Get-Item $out).Length / 1MB))
    }
    $targets = if ($All) { @('windows/amd64', 'linux/amd64', 'linux/arm64', 'darwin/amd64', 'darwin/arm64') } else { @('') }
    $dist = Join-Path $root 'dist'
    New-Item -ItemType Directory -Force $dist | Out-Null
    foreach ($t in $targets) {
        if ($t) {
            $env:GOOS, $env:GOARCH = $t.Split('/')
            $name = "ferry-$($env:GOOS)-$($env:GOARCH)"
            if ($env:GOOS -eq 'windows') { $name += '.exe' }
        } else {
            $env:GOOS = $saved.GOOS; $env:GOARCH = $saved.GOARCH
            $name = 'ferry.exe'
        }
        $out = Join-Path $dist $name
        & go build -trimpath -ldflags "-s -w -X main.Version=$Version" -o $out ./cmd/ferry
        if ($LASTEXITCODE) { throw "build ferry $t failed" }
        Write-Host ("ferry  {0,-14} {1,8:N1} MB  {2}" -f $(if ($t) { $t } else { 'host' }), ((Get-Item $out).Length / 1MB), $out)
    }
} finally {
    $env:GOOS = $saved.GOOS; $env:GOARCH = $saved.GOARCH; $env:CGO_ENABLED = $saved.CGO_ENABLED
}
