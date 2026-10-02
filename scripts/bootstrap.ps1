$ErrorActionPreference = 'Stop'
Set-Location (Join-Path $PSScriptRoot '..')
if ((Test-Path '.env') -or (Test-Path 'secrets')) { throw 'Existing .env or secrets directory: preserve it and configure manually.' }
function New-Token { $bytes = New-Object byte[] 32; [Security.Cryptography.RandomNumberGenerator]::Fill($bytes); return [Convert]::ToHexString($bytes).ToLowerInvariant() }
New-Item -ItemType Directory 'secrets' | Out-Null
$dbSecret = New-Token
$storageSecret = New-Token
$apiSecret = New-Token
$workerSecret = New-Token
$utf8 = New-Object Text.UTF8Encoding $false
[IO.File]::WriteAllText((Join-Path (Get-Location) '.env'), "POSTGRES_PASSWORD=$dbSecret`nS3_SECRET_KEY=$storageSecret`n", $utf8)
[IO.File]::WriteAllText((Join-Path (Get-Location) 'secrets/api-tokens.json'), (@{local=$apiSecret} | ConvertTo-Json), $utf8)
[IO.File]::WriteAllText((Join-Path (Get-Location) 'secrets/worker-token'), $workerSecret, $utf8)
[IO.File]::WriteAllText((Join-Path (Get-Location) 'secrets/repositories.json'), '{}', $utf8)
[IO.File]::WriteAllText((Join-Path (Get-Location) 'secrets/model-key'), '', $utf8)
New-Item -ItemType Directory 'secrets/publication' | Out-Null
[IO.File]::WriteAllText((Join-Path (Get-Location) 'secrets/publication/repositories.json'), '{}', $utf8)
Write-Host 'Created configuration. API token is in secrets/api-tokens.json; add model key to secrets/model-key.'
