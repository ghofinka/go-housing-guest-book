# Loads .env.example (defaults) then .env (overrides) into this process's
# environment, mirroring the Makefile's `include .env.example` / `-include .env`
# order, then runs the API. Use this on Windows where `make` is unavailable.
#
#   ./scripts/run.ps1              # runs: go run ./cmd/api
#   ./scripts/run.ps1 ./cmd/seed  # run a different package with the same env
[CmdletBinding()]
param(
    [string]$Package = "./cmd/api"
)

$ErrorActionPreference = "Stop"

# Resolve the repo root (this script lives in <root>/scripts) and work from there.
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

function Import-DotEnv([string]$Path) {
    if (-not (Test-Path $Path)) { return }
    foreach ($line in Get-Content $Path) {
        # Skip blank lines and comments.
        if ($line -notmatch '^\s*[^#\s][^=]*=') { continue }
        $k, $v = $line -split '=', 2
        $k = $k.Trim()
        $v = $v.Trim()
        # Strip one layer of surrounding quotes, matching how the app treats values.
        if ($v.Length -ge 2 -and (($v[0] -eq '"' -and $v[-1] -eq '"') -or ($v[0] -eq "'" -and $v[-1] -eq "'"))) {
            $v = $v.Substring(1, $v.Length - 2)
        }
        Set-Item "env:$k" $v
    }
}

Import-DotEnv (Join-Path $root ".env.example")  # defaults
Import-DotEnv (Join-Path $root ".env")          # real values override

if ([string]::IsNullOrEmpty($env:DATABASE_URL)) {
    Write-Error "DATABASE_URL is empty after loading .env - fill it in .env"
}

go run $Package
