# Shared version files for the Windows packaging and GitHub release scripts.
$desktopVersionPatterns = [ordered]@{
    'desktop/package.json' = @('("version"\s*:\s*")([^"]+)(")')
    'desktop/package-lock.json' = @(
        '(\A\s*\{[^{}]*?"version"\s*:\s*")([^"]+)(")'
        '("packages"\s*:\s*\{\s*""\s*:\s*\{[^{}]*?"version"\s*:\s*")([^"]+)(")'
    )
    'desktop/src-tauri/tauri.conf.json' = @('("version"\s*:\s*")([^"]+)(")')
    'desktop/src-tauri/Cargo.toml' = @('(?m)(^\[package\][^\r\n]*\r?\n(?:(?!^\[)[^\r\n]*\r?\n)*?^version\s*=\s*")([^"]+)(")')
    'desktop/src-tauri/Cargo.lock' = @('(?m)(^name = "ferry-desktop"\r?\nversion = ")([^"]+)(")')
}

function Read-DesktopVersionFiles([string]$Root, [switch]$AllowMissingCargoLock) {
    foreach ($file in $desktopVersionPatterns.Keys) {
        $path = Join-Path $Root $file
        if ($AllowMissingCargoLock -and $file -eq 'desktop/src-tauri/Cargo.lock' -and -not (Test-Path -LiteralPath $path)) { continue }
        $text = [IO.File]::ReadAllText($path)
        $versions = @(
            foreach ($pattern in $desktopVersionPatterns[$file]) {
                $match = [regex]::Match($text, $pattern)
                if (-not $match.Success) { throw "读不到 $file 里的版本号" }
                $match.Groups[2].Value
            }
        )
        [pscustomobject]@{ File = $file; Path = $path; Text = $text; Versions = $versions }
    }
}

function Next-DesktopVersion([string]$Current) {
    $v = [version]$Current
    # Keep the existing package.ps1 rollover at patch 10.
    if ($v.Build -ge 9) { return "$($v.Major).$($v.Minor + 1).0" }
    return "$($v.Major).$($v.Minor).$($v.Build + 1)"
}

function Write-DesktopVersions($Files, [string]$Version, [switch]$Restore) {
    $utf8 = New-Object System.Text.UTF8Encoding($false)
    if (-not $Restore) {
        foreach ($entry in $Files) {
            if ([IO.File]::ReadAllText($entry.Path) -cne $entry.Text) {
                throw "$($entry.File) 在确认前后发生了变化，请检查后重试"
            }
        }
    }
    foreach ($entry in $Files) {
        $text = $entry.Text
        foreach ($pattern in $desktopVersionPatterns[$entry.File]) {
            $value = [regex]::Match($text, $pattern).Groups[2]
            if (-not $value.Success) { throw "读不到 $($entry.File) 里的版本号" }
            $text = $text.Remove($value.Index, $value.Length).Insert($value.Index, $Version)
        }
        if ($text -ceq $entry.Text) { continue }
        if ($Restore) {
            if ([IO.File]::ReadAllText($entry.Path) -ceq $text) {
                [IO.File]::WriteAllText($entry.Path, $entry.Text, $utf8)
            }
        } else {
            [IO.File]::WriteAllText($entry.Path, $text, $utf8)
        }
    }
}
