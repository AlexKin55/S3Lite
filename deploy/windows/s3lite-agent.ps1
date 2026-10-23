# Install the S3Lite agent as a Windows service using NSSM.
# Prerequisites: NSSM (https://nssm.cc), Go binaries bin\agent.exe, bin\weed.exe.
#
#   powershell -ExecutionPolicy Bypass -File s3lite-agent.ps1 -ControlPlane 192.168.1.9
#
param(
    [Parameter(Mandatory=$true)][string]$ControlPlane,
    [int]$Port = 8080
)

$Root = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$AgentBin = Join-Path $Root "bin\agent.exe"
$WeedBin = Join-Path $Root "bin\weed.exe"
$DataDir = "$env:ProgramData\S3Lite\agent"
$Nssm = Join-Path $Root "bin\nssm.exe"

if (-not (Test-Path $Nssm)) {
    Write-Host "nssm.exe not found in bin\. Download from https://nssm.cc" -ForegroundColor Red
    exit 1
}

New-Item -ItemType Directory -Force -Path $DataDir | Out-Null

& $Nssm install S3LiteAgent $AgentBin "-dir=$DataDir" `
    "-server=https://$($ControlPlane):8099" `
    "-port=$Port" "-weed=$WeedBin" "-insecure"
& $Nssm set S3LiteAgent AppDirectory (Split-Path $AgentBin)
& $Nssm set S3LiteAgent Start SERVICE_AUTO_START
& $Nssm start S3LiteAgent

Write-Host "S3LiteAgent service installed and started." -ForegroundColor Green
