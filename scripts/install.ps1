param(
    [string]$ApiUrl = 'http://localhost:8080',
    [string]$InstallDir = (Join-Path $env:LOCALAPPDATA 'dcar/bin'),
    [switch]$NoPath
)
$ErrorActionPreference = 'Stop'
$projectRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$installPath = [IO.Path]::GetFullPath($InstallDir)
$tokenPath = Join-Path $projectRoot 'secrets/api-tokens.json'
if (!(Test-Path -LiteralPath $tokenPath)) { throw 'Run bootstrap in the project first, or configure a remote API separately.' }
New-Item -ItemType Directory -Force -Path $installPath | Out-Null
$binaryPath = Join-Path $installPath 'dcar.exe'
Push-Location $projectRoot
try {
    go build -o $binaryPath ./cmd/dcar
    if ($LASTEXITCODE -ne 0) { throw 'Client build failed.' }
    & $binaryPath configure --url $ApiUrl --token-file $tokenPath
    if ($LASTEXITCODE -ne 0) { throw 'Client configuration failed.' }
} finally { Pop-Location }
if (!$NoPath) {
    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    $entries = @($userPath -split ';' | Where-Object { $_ })
    if (!($entries | Where-Object { $_.TrimEnd('\') -ieq $installPath.TrimEnd('\') })) {
        [Environment]::SetEnvironmentVariable('Path', (($entries + $installPath) -join ';'), 'User')
    }
    if (!(($env:Path -split ';') | Where-Object { $_.TrimEnd('\') -ieq $installPath.TrimEnd('\') })) { $env:Path += ';' + $installPath }
}
Write-Host 'Installed dcar. Run dcar in any GitHub working directory. Open a new terminal for the updated user PATH.'
