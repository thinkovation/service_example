<#
.SYNOPSIS
    The Makefile's commands, for Windows where make is usually missing.

.DESCRIPTION
    Same three modes as the Makefile: dev runs the service on this machine
    against a local SQLite file, stage runs the service and Postgres as
    containers, prod builds the service image.

.EXAMPLE
    .\scripts\run.ps1 dev
    .\scripts\run.ps1 stage-up
    .\scripts\run.ps1 test
    .\scripts\run.ps1 help
#>
[CmdletBinding()]
param(
    [Parameter(Position = 0)]
    [string]$Command = 'help',

    # Host port the service is published on.
    [int]$Port = 8080,

    # Image name for the prod build.
    [string]$Image = 'customersvc',

    # Registry for prod-push, e.g. ghcr.io/thinkovation.
    [string]$Registry = ''
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

# Everything runs from the repository root, whichever directory you call from.
$RepoRoot = Split-Path -Parent $PSScriptRoot
Push-Location $RepoRoot
try {

function Get-BuildInfo {
    $version = 'dev'
    $commit = 'unknown'
    try {
        $version = (git describe --tags --always --dirty 2>$null)
        $commit = (git rev-parse --short HEAD 2>$null)
    } catch {
        # Not a git checkout, or git is not installed: the defaults stand.
    }
    if (-not $version) { $version = 'dev' }
    if (-not $commit) { $commit = 'unknown' }

    [pscustomobject]@{
        Version   = $version
        Commit    = $commit
        BuildDate = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
    }
}

# Set-DevEnvironment applies the dev defaults, then anything in .env, which wins.
function Set-DevEnvironment {
    $env:SERVICE_ENV = 'development'
    $env:STORE_DRIVER = 'sqlite'
    $env:SQLITE_DSN = 'file:./data/customers.db'
    $env:HTTP_ADDR = ":$Port"
    $env:LOG_FORMAT = 'text'
    $env:LOG_LEVEL = 'debug'

    if (Test-Path .env) {
        foreach ($line in Get-Content .env) {
            $trimmed = $line.Trim()
            if ($trimmed -eq '' -or $trimmed.StartsWith('#')) { continue }

            $name, $value = $trimmed -split '=', 2
            if ($null -ne $value) {
                Set-Item -Path "env:$($name.Trim())" -Value $value.Trim().Trim('"')
            }
        }
    }
}

# Invoke-Checked runs a command and stops the script if it fails, so a broken
# build does not look like a successful one.
function Invoke-Checked {
    param([string]$Exe, [string[]]$Arguments)

    & $Exe @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "$Exe $($Arguments -join ' ') exited with $LASTEXITCODE"
    }
}

function Invoke-StageCompose {
    param([string[]]$Arguments)

    $build = Get-BuildInfo
    $env:IMAGE = $Image
    $env:VERSION = $build.Version
    $env:COMMIT = $build.Commit
    $env:BUILD_DATE = $build.BuildDate
    $env:HTTP_PORT = "$Port"

    Invoke-Checked docker (@('compose', '-f', 'deploy/compose.stage.yaml') + $Arguments)
}

function Show-Help {
    @"
Usage: .\scripts\run.ps1 <command> [-Port 8080] [-Image customersvc] [-Registry ghcr.io/you]

Dev - this machine, local SQLite file
  dev             Run the service against .\data\customers.db
  dev-build       Build .\bin\customersvc.exe with version stamps
  dev-run         Build, then run that binary
  dev-reset       Delete the local SQLite database
  dev-smoke       Check a running dev service answers on every endpoint

Stage - service container plus Postgres container
  stage-up        Build and start the stack, waiting until it is healthy
  stage-down      Stop it, keeping the database volume
  stage-clean     Stop it and delete the database volume
  stage-logs      Follow the service log
  stage-ps        Show the containers and their health
  stage-smoke     Check the stage stack answers on every endpoint
  stage-config    Render the compose file with variables resolved

Prod - the service as a container image
  prod-build      Build the image, tagged with the version and latest
  prod-up         Run the image against deploy\prod.env
  prod-down       Stop it
  prod-push       Push the image (needs -Registry)

Quality
  test            Run the tests
  test-postgres   Run the tests against SQLite and Postgres (needs stage-up)
  cover           Report test coverage
  fmt             Format the source
  vet             Run go vet
  check           Format, vet and test
"@
}

switch ($Command) {
    'dev' {
        Set-DevEnvironment
        Invoke-Checked go @('run', './cmd/customersvc')
    }
    'dev-build' {
        $build = Get-BuildInfo
        $module = 'github.com/thinkovation/service_example'
        $ldflags = "-s -w " +
            "-X $module/internal/buildinfo.Version=$($build.Version) " +
            "-X $module/internal/buildinfo.Commit=$($build.Commit) " +
            "-X $module/internal/buildinfo.BuildDate=$($build.BuildDate)"
        $env:CGO_ENABLED = '0'
        Invoke-Checked go @('build', '-trimpath', '-ldflags', $ldflags, '-o', 'bin/customersvc.exe', './cmd/customersvc')
        Write-Host 'built bin\customersvc.exe'
    }
    'dev-run' {
        & $PSCommandPath -Command dev-build -Port $Port
        Set-DevEnvironment
        Invoke-Checked '.\bin\customersvc.exe' @()
    }
    'dev-reset' {
        Remove-Item -Force -ErrorAction SilentlyContinue data\customers.db, data\customers.db-wal, data\customers.db-shm
        Write-Host 'local database removed'
    }
    'dev-smoke'   { Invoke-Checked go @('run', './cmd/smoke', "http://localhost:$Port") }

    'stage-up' {
        Invoke-StageCompose @('up', '--build', '--detach', '--wait')
        Write-Host "stage is up on http://localhost:$Port"
    }
    'stage-down'   { Invoke-StageCompose @('down') }
    'stage-clean'  { Invoke-StageCompose @('down', '--volumes') }
    'stage-logs'   { Invoke-StageCompose @('logs', '--follow', 'customersvc') }
    'stage-ps'     { Invoke-StageCompose @('ps') }
    'stage-config' { Invoke-StageCompose @('config') }
    'stage-smoke'  { Invoke-Checked go @('run', './cmd/smoke', "http://localhost:$Port") }

    'prod-build' {
        $build = Get-BuildInfo
        Invoke-Checked docker @(
            'build',
            '--build-arg', "VERSION=$($build.Version)",
            '--build-arg', "COMMIT=$($build.Commit)",
            '--build-arg', "BUILD_DATE=$($build.BuildDate)",
            '--tag', "$($Image):$($build.Version)",
            '--tag', "$($Image):latest",
            '.'
        )
    }
    'prod-up' {
        if (-not (Test-Path deploy\prod.env)) {
            throw 'deploy\prod.env is missing; copy deploy\prod.env.example and fill it in'
        }
        $env:IMAGE = $Image
        $env:HTTP_PORT = "$Port"
        Invoke-Checked docker @('compose', '-f', 'deploy/compose.prod.yaml', 'up', '--detach', '--wait')
    }
    'prod-down' {
        Invoke-Checked docker @('compose', '-f', 'deploy/compose.prod.yaml', 'down')
    }
    'prod-push' {
        if (-not $Registry) { throw 'set -Registry, e.g. .\scripts\run.ps1 prod-push -Registry ghcr.io/thinkovation' }
        $build = Get-BuildInfo
        Invoke-Checked docker @('tag', "$($Image):$($build.Version)", "$Registry/$($Image):$($build.Version)")
        Invoke-Checked docker @('push', "$Registry/$($Image):$($build.Version)")
    }

    'test'          { Invoke-Checked go @('test', '-race', '-count=1', './...') }
    'test-postgres' {
        $env:TEST_POSTGRES_DSN = 'postgres://customers:customers@localhost:5432/customers?sslmode=disable'
        Invoke-Checked go @('test', '-race', '-count=1', './...')
    }
    'cover' {
        Invoke-Checked go @('test', '-count=1', '-coverprofile=coverage.out', './...')
        Invoke-Checked go @('tool', 'cover', '-func=coverage.out')
    }
    'fmt'   { Invoke-Checked gofmt @('-s', '-w', '.') }
    'vet'   { Invoke-Checked go @('vet', './...') }
    'check' {
        & $PSCommandPath -Command fmt
        & $PSCommandPath -Command vet
        & $PSCommandPath -Command test
    }

    'help'  { Show-Help }
    default {
        Write-Host "unknown command '$Command'`n"
        Show-Help
        exit 2
    }
}

} finally {
    Pop-Location
}
