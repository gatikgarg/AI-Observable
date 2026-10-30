<#
.SYNOPSIS
    Builds and starts AI-Observable (API Manager, proxy and Grafana) with Docker.
#>

param(
    [switch]$Open
)

$ErrorActionPreference = "Stop"
$RepoRoot = Split-Path -Parent $MyInvocation.MyCommand.Definition
Set-Location $RepoRoot

try {
    $null = & docker compose version 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "Docker Compose is not available."
    }
    $null = & docker info 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "Docker is not running."
    }
} catch {
    Write-Host "Docker Desktop with Compose v2 is required." -ForegroundColor Red
    Write-Host "Start Docker Desktop, then run this command again." -ForegroundColor Yellow
    exit 1
}

Write-Host "Building and starting AI-Observable..." -ForegroundColor Cyan
docker compose up --build -d
if ($LASTEXITCODE -ne 0) {
    Write-Error "Docker Compose failed to start AI-Observable."
    exit 1
}

Write-Host "Waiting for the API Manager and Grafana..." -ForegroundColor Cyan
$deadline = (Get-Date).AddSeconds(150)
$apiReady = $false
$grafanaReady = $false
while ((Get-Date) -lt $deadline) {
    try {
        $null = Invoke-WebRequest -UseBasicParsing -TimeoutSec 3 http://127.0.0.1:7879/healthz
        $apiReady = $true
    } catch {}
    try {
        $health = Invoke-RestMethod -TimeoutSec 3 http://127.0.0.1:3000/api/health
        $grafanaReady = $health.database -eq "ok"
    } catch {}
    if ($apiReady -and $grafanaReady) {
        break
    }
    Start-Sleep -Seconds 3
}

if (-not ($apiReady -and $grafanaReady)) {
    Write-Host "Services did not become healthy. Recent logs:" -ForegroundColor Red
    docker compose logs --tail 80
    exit 1
}

Write-Host @"
==============================================================
 AI-Observable is ready!
 - API Manager (add keys):  http://localhost:7879
 - Grafana Dashboards:      http://localhost:3000 (admin / admin)
 - Proxy Endpoint:          http://localhost:7878/<provider>/v1
 - Stop everything:         docker compose down
==============================================================
"@ -ForegroundColor Green

if ($Open) {
    Start-Process "http://localhost:7879"
}
