<#
.SYNOPSIS
    Starts AI-Observable: Grafana in Docker + the local proxy/API Manager daemon.
#>

param(
    [switch]$Build
)

$ErrorActionPreference = "Stop"
$RepoRoot = Split-Path -Parent $MyInvocation.MyCommand.Definition
Set-Location $RepoRoot

# 1. Build if requested or if llmtap.exe doesn't exist
$ExePath = Join-Path $RepoRoot "llmtap.exe"
if ($Build -or !(Test-Path $ExePath)) {
    Write-Host "Building llmtap.exe..." -ForegroundColor Cyan
    $GoExe = Join-Path $RepoRoot ".tools\go\bin\go.exe"
    if (!(Test-Path $GoExe)) {
        $GoExe = "go"
    }
    
    $env:GOCACHE = Join-Path $RepoRoot ".cache\go-build"
    $env:GOMODCACHE = Join-Path $RepoRoot ".cache\go-mod"
    $env:GOPATH = Join-Path $RepoRoot ".cache\go-path"
    
    & $GoExe build -buildvcs=false -trimpath -ldflags "-s -w -X main.version=dev" -o $ExePath .\cmd\llmtap
    if ($LASTEXITCODE -ne 0) {
        Write-Error "Build failed!"
        exit 1
    }
    Write-Host "Build complete: $ExePath" -ForegroundColor Green
}

# 2. Check Docker
Write-Host "Checking Docker..." -ForegroundColor Cyan
$dockerRunning = $false
try {
    $null = & docker ps 2>&1
    if ($LASTEXITCODE -eq 0) {
        $dockerRunning = $true
    }
} catch {}

if (-not $dockerRunning) {
    Write-Warning "Docker daemon does not appear to be running. Please make sure Docker Desktop is launched and ready."
    exit 1
}

# 3. Setup Grafana view directory (live data)
$ViewDir = "$env:USERPROFILE\.llmtap\view"
if (!(Test-Path $ViewDir)) {
    New-Item -ItemType Directory -Path $ViewDir -Force | Out-Null
}
Write-Host "Running in LIVE mode (data from $ViewDir)..." -ForegroundColor Green
$env:LLMTAP_VIEW_DIR = $ViewDir

# 4. Start Grafana in Docker
Write-Host "Starting Grafana container..." -ForegroundColor Cyan
docker compose -f deploy/docker-compose.yml up -d
if ($LASTEXITCODE -ne 0) {
    Write-Error "Failed to start Grafana via Docker Compose"
    exit 1
}

Write-Host @"
==============================================================
 AI-Observable is ready!
 - API Manager (add keys):  http://localhost:7879
 - Grafana Dashboards:      http://localhost:3000 (admin / admin)
 - Proxy Endpoint:          http://localhost:7878/<provider>/v1
==============================================================
Starting local llmtap collector (Press Ctrl+C to stop)...
"@ -ForegroundColor Green

# 5. Run llmtap daemon
& $ExePath start
