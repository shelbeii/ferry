# Release script integration checks against disposable local Git repositories; nothing is sent to GitHub.
# Run: powershell -NoProfile -ExecutionPolicy Bypass -File scripts\test-release.ps1
$ErrorActionPreference = 'Stop'
$releaseScript = Join-Path $PSScriptRoot 'release.ps1'
$shell = (Get-Process -Id $PID).Path
$realGit = (Get-Command git -CommandType Application | Select-Object -First 1).Source.Replace("'", "''")
$tempParent = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\', '/')
$testRoot = Join-Path $tempParent ('ferry-release-tests-' + [Guid]::NewGuid().ToString('N'))
$utf8 = New-Object System.Text.UTF8Encoding($false)
$checks = 0

function Write-TestFile([string]$path, [string]$content) {
    New-Item -ItemType Directory -Force (Split-Path -Parent $path) | Out-Null
    [IO.File]::WriteAllText($path, $content, $utf8)
}

function Test-Git([string[]]$GitArgs) {
    $savedPreference = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        $output = @(& git @GitArgs 2>&1)
        $code = $LASTEXITCODE
    } finally { $ErrorActionPreference = $savedPreference }
    if ($code -ne 0) { throw "Git test setup failed: git $($GitArgs -join ' ')`n$($output -join "`n")" }
    $output | ForEach-Object { $_.ToString() }
}

function Assert([bool]$condition, [string]$message) {
    if (-not $condition) { throw $message }
    $script:checks++
    Write-Host "PASS: $message"
}

function New-Fixture([string]$name) {
    $base = Join-Path $testRoot $name
    $repo = Join-Path $base 'repo'
    $bare = Join-Path $base 'remote.git'
    New-Item -ItemType Directory -Force $repo | Out-Null
    Test-Git -GitArgs @('init', '--bare', '--initial-branch=main', $bare) | Out-Null
    Test-Git -GitArgs @('init', '--initial-branch=main', $repo) | Out-Null
    Test-Git -GitArgs @('-C', $repo, 'config', 'user.name', 'Release Test') | Out-Null
    Test-Git -GitArgs @('-C', $repo, 'config', 'user.email', 'release-test@example.invalid') | Out-Null
    $remote = 'https://github.com/ferry-release-tests/ferry.git'
    # Repository-local URL rewriting keeps all fetches/pushes inside the fixture.
    Test-Git -GitArgs @('-C', $repo, 'config', ('url.' + $bare.Replace('\', '/') + '.insteadOf'), $remote) | Out-Null
    Test-Git -GitArgs @('-C', $repo, 'remote', 'add', 'origin', $remote) | Out-Null
    $fixtureScript = Join-Path $repo 'scripts\release.ps1'
    New-Item -ItemType Directory -Force (Split-Path -Parent $fixtureScript) | Out-Null
    Copy-Item -LiteralPath $releaseScript -Destination $fixtureScript
    # get-url expands insteadOf, so emulate only its displayed URL; all real Git I/O stays local.
    $bootstrap = @'
param([switch]$DryRun, [switch]$Yes, [switch]$NoOpen)
function git {
    if (($args -join ' ') -eq 'remote get-url origin') {
        $global:LASTEXITCODE = 0
        return 'https://github.com/ferry-release-tests/ferry.git'
    }
    if ($env:FERRY_RELEASE_TEST_FAIL_QUERY -and $args[0] -eq $env:FERRY_RELEASE_TEST_FAIL_QUERY) {
        $global:LASTEXITCODE = 128
        return 'Simulated remote query failure'
    }
    & '__GIT__' @args
    $global:LASTEXITCODE = $LASTEXITCODE
}
& (Join-Path $PSScriptRoot 'release.ps1') @PSBoundParameters
exit $LASTEXITCODE
'@
    $runner = Join-Path $repo 'scripts\run-test-release.ps1'
    Write-TestFile $runner $bootstrap.Replace('__GIT__', $realGit)
    Write-TestFile (Join-Path $repo 'desktop\package.json') '{"version":"1.2.3"}'
    Write-TestFile (Join-Path $repo 'desktop\package-lock.json') '{"version":"1.2.3","packages":{"":{"version":"1.2.3"}}}'
    Write-TestFile (Join-Path $repo 'desktop\src-tauri\tauri.conf.json') '{"version":"1.2.3"}'
    Write-TestFile (Join-Path $repo 'desktop\src-tauri\Cargo.toml') "[package]`nname = `"ferry-desktop`"`nversion = `"1.2.3`"`n"
    Write-TestFile (Join-Path $repo 'desktop\src-tauri\Cargo.lock') "[[package]]`nname = `"ferry-desktop`"`nversion = `"1.2.3`"`n"
    Test-Git -GitArgs @('-C', $repo, 'add', '.') | Out-Null
    Test-Git -GitArgs @('-C', $repo, 'commit', '-m', 'Initial release fixture') | Out-Null
    Test-Git -GitArgs @('-C', $repo, 'tag', 'v1.2.2') | Out-Null
    Write-TestFile (Join-Path $repo 'README.md') 'New release content'
    Test-Git -GitArgs @('-C', $repo, 'add', 'README.md') | Out-Null
    Test-Git -GitArgs @('-C', $repo, 'commit', '-m', 'Release changes') | Out-Null
    Test-Git -GitArgs @('-C', $repo, 'push', '-u', 'origin', 'main', '--tags') | Out-Null
    return @{ Repo = $repo; Bare = $bare; Script = $runner }
}

function Run-Release($fixture, [string[]]$ScriptArgs, [int]$want) {
    $savedPreference = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        $output = @(& $shell -NoProfile -ExecutionPolicy Bypass -File $fixture.Script @ScriptArgs 2>&1)
        $code = $LASTEXITCODE
    } finally { $ErrorActionPreference = $savedPreference }
    if ($code -ne $want) { throw "Release returned $code, expected $want`n$($output -join "`n")" }
}

try {
    $f = New-Fixture 'normal'
    Run-Release $f @('-DryRun') 0
    Assert (-not (Test-Git -GitArgs @('-C', $f.Repo, 'tag', '--list', 'v1.2.3'))) 'DryRun does not create a local tag'
    Assert (-not (Test-Git -GitArgs @('-C', $f.Repo, 'ls-remote', '--tags', 'origin', 'refs/tags/v1.2.3'))) 'DryRun does not push a tag'

    Write-TestFile (Join-Path $f.Repo '.claude\local.txt') 'Local tool state'
    Run-Release $f @('-DryRun') 0
    Assert $true 'Local .claude files do not block release'
    Write-TestFile (Join-Path $f.Repo 'new.go') 'package main'
    Run-Release $f @('-DryRun') 1
    Remove-Item -LiteralPath (Join-Path $f.Repo 'new.go')
    Assert $true 'Untracked source files block release'

    $conf = Join-Path $f.Repo 'desktop\src-tauri\tauri.conf.json'
    Write-TestFile $conf '{"version":"1.2.4"}'
    Run-Release $f @('-DryRun') 1
    Write-TestFile $conf '{"version":"1.2.3"}'
    Assert $true 'Inconsistent desktop versions block release'

    Write-TestFile (Join-Path $f.Repo 'README.md') 'Uncommitted changes'
    Run-Release $f @('-DryRun') 1
    Write-TestFile (Join-Path $f.Repo 'README.md') 'New release content'
    Assert $true 'Uncommitted changes block release'

    $savedQuery = $env:FERRY_RELEASE_TEST_FAIL_QUERY
    try {
        $env:FERRY_RELEASE_TEST_FAIL_QUERY = 'ls-remote'
        Run-Release $f @('-Yes', '-NoOpen') 1
    } finally { $env:FERRY_RELEASE_TEST_FAIL_QUERY = $savedQuery }
    Assert (-not (Test-Git -GitArgs @('-C', $f.Repo, 'tag', '--list', 'v1.2.3'))) 'Remote query failure never creates a release tag'

    Test-Git -GitArgs @('-C', $f.Repo, 'tag', '-a', 'v1.2.3', '-m', 'Previously failed push') | Out-Null
    Run-Release $f @('-DryRun') 0
    Assert $true 'A local tag at HEAD can be retried'
    Run-Release $f @('-Yes', '-NoOpen') 0
    $head = Test-Git -GitArgs @('-C', $f.Repo, 'rev-parse', 'HEAD')
    $tagCommit = Test-Git -GitArgs @('--git-dir', $f.Bare, 'rev-parse', 'refs/tags/v1.2.3^{}')
    Assert ($head -eq $tagCommit) 'Release pushes the annotated tag at HEAD'
    Run-Release $f @('-Yes', '-NoOpen') 0
    Assert $true 'An existing remote tag is handled without recreating it'

    $ahead = New-Fixture 'ahead'
    Write-TestFile (Join-Path $ahead.Repo 'README.md') 'New local commit'
    Test-Git -GitArgs @('-C', $ahead.Repo, 'add', 'README.md') | Out-Null
    Test-Git -GitArgs @('-C', $ahead.Repo, 'commit', '-m', 'Not yet pushed') | Out-Null
    Run-Release $ahead @('-Yes', '-NoOpen') 0
    $head = Test-Git -GitArgs @('-C', $ahead.Repo, 'rev-parse', 'HEAD')
    $remoteHead = Test-Git -GitArgs @('--git-dir', $ahead.Bare, 'rev-parse', 'refs/heads/main')
    Assert ($head -eq $remoteHead) 'Release pushes outstanding main commits first'

    $wrongTag = New-Fixture 'wrong-tag'
    Test-Git -GitArgs @('-C', $wrongTag.Repo, 'tag', '-a', 'v1.2.3', 'HEAD~1', '-m', 'Wrong commit') | Out-Null
    Run-Release $wrongTag @('-DryRun') 1
    Assert $true 'A local release tag at another commit is rejected'
    Write-Host "`n$checks release checks passed."
} finally {
    $resolved = [IO.Path]::GetFullPath($testRoot)
    if ((Split-Path -Parent $resolved) -ne $tempParent -or (Split-Path -Leaf $resolved) -notlike 'ferry-release-tests-*') {
        throw "Unsafe temporary cleanup path: $resolved"
    }
    if (Test-Path -LiteralPath $resolved) { Remove-Item -LiteralPath $resolved -Recurse -Force }
}
