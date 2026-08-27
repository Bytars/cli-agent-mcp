# SPDX-License-Identifier: Apache-2.0
<#
.SYNOPSIS
Runs the end-to-end suite against a real CLI agent.

.DESCRIPTION
CI cannot run this. GitHub's runners have no Claude Code install and no API key,
and putting one there is not on the table, so CI proves compilation, vet, the
unit tests and the mock smoke test — everything that does not need a real agent.

That leaves a category of bug CI is structurally unable to see. The mock agent
never reads --mcp-config, never asks for permission, and never spends a token,
so anything that only breaks when a real worker runs looks green all the way to
release. One such bug shipped exactly that way: a relative --mcp-config path
made every agent_run_task fail while the suite reported PASSED.

This script is the gate for that category. Run it before opening a PR that
touches how workers are launched, approved, or accounted for.

.PARAMETER Agent
Which agent to drive. Defaults to claude.

.PARAMETER Workspace
A scratch git repository the agent may modify. One is created under the system
temp directory if this is not given. It must not be a repository whose contents
you care about: scenarios tell the agent to create and edit files in it.

.PARAMETER Only
Run a single scenario instead of the whole matrix.

.EXAMPLE
./scripts/e2e.ps1
.EXAMPLE
./scripts/e2e.ps1 -Only permission -Workspace D:\scratch\e2e
#>
[CmdletBinding()]
param(
    [string] $Agent = 'claude',
    [string] $Workspace,
    [string] $Only
)

$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot
Set-Location $repo

# Evidence, not just a verdict. A scenario that fails against a real agent is
# usually diagnosed from the transcript rather than the assertion that tripped.
$stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
$logDir = Join-Path $repo ".e2e/$stamp"
New-Item -ItemType Directory -Force -Path $logDir | Out-Null

function Write-Head([string] $text) {
    Write-Host ''
    Write-Host "=== $text ===" -ForegroundColor Cyan
}

if (-not $Workspace) {
    $Workspace = Join-Path ([System.IO.Path]::GetTempPath()) "cli-agent-mcp-e2e-$stamp"
}

if (-not (Test-Path (Join-Path $Workspace '.git'))) {
    Write-Head "Creating scratch repository at $Workspace"
    New-Item -ItemType Directory -Force -Path $Workspace | Out-Null
    Push-Location $Workspace
    try {
        git init -q -b main
        git config user.email 'e2e@example.com'
        git config user.name 'E2E'
        git config commit.gpgsign false
        Set-Content -Encoding utf8 -Path 'README.md' -Value "# E2E scratch`n`nA throwaway repository for driving real agents.`n"
        git add -A
        git commit -q -m 'initial'
    } finally { Pop-Location }
}

Write-Head 'Building'
go build -o cli-agent-mcp.exe .
if ($LASTEXITCODE -ne 0) { throw 'build failed' }

# What CI already covers. Repeated here so one command answers "is this
# releasable", rather than leaving the cheap checks to be remembered separately.
$suite = @(
    @{ Name = 'gofmt';   Needs = 'none'; Script = {
        $bad = (gofmt -l .) -join "`n"
        if ($bad) { throw "not gofmt-clean:`n$bad" } } },
    @{ Name = 'vet';     Needs = 'none'; Script = { go vet ./...;  if ($LASTEXITCODE) { throw 'vet failed' } } },
    @{ Name = 'unit';    Needs = 'none'; Script = { go test ./...; if ($LASTEXITCODE) { throw 'unit tests failed' } } },
    @{ Name = 'mock';    Needs = 'none'; Script = { Invoke-Scenario -Scenario '' -UseAgent 'mock' } },

    # The cap is enforced server-side and does not depend on what a worker does,
    # so the mock proves it more cheaply and just as truthfully.
    @{ Name = 'concurrency'; Needs = 'none'; Script = { Invoke-Scenario -Scenario 'concurrency' -UseAgent 'mock' -Vars @{ CLI_AGENT_MCP_MAX_CONCURRENT = '3' } } },

    # Two server processes over one state directory. Server-side, so the mock
    # proves it; the state directory is a scratch one because the scenario
    # cancels what it finds and must never be pointed at real tasks.
    @{ Name = 'crosscancel'; Needs = 'none'; Script = {
        $dir = Join-Path $logDir 'xcancel-state'
        New-Item -ItemType Directory -Force -Path $dir | Out-Null
        Invoke-Scenario -Scenario 'crosscancel' -UseAgent 'mock' -Vars @{ CLI_AGENT_MCP_STATE_DIR = $dir } } },

    @{ Name = 'real';       Needs = 'agent'; Script = { Invoke-Scenario -Scenario '' -UseAgent $Agent } },
    @{ Name = 'permission'; Needs = 'agent'; Script = { Invoke-Scenario -Scenario 'permission' -UseAgent $Agent -Vars @{ CLI_AGENT_MCP_WATCH_WINDOW_SECONDS = '10'; CLI_AGENT_MCP_PERMISSION_MODE = 'default' } } },
    @{ Name = 'abandon';    Needs = 'agent'; Script = { Invoke-Scenario -Scenario 'abandon'    -UseAgent $Agent -Vars @{ CLI_AGENT_MCP_WATCH_WINDOW_SECONDS = '10' } } },
    @{ Name = 'worktree';   Needs = 'agent'; Script = { Invoke-Scenario -Scenario 'worktree'   -UseAgent $Agent -Vars @{ CLI_AGENT_MCP_WATCH_WINDOW_SECONDS = '10' } } }
)

function Invoke-Scenario {
    param(
        [string] $Scenario,
        [string] $UseAgent,
        [hashtable] $Vars = @{}
    )

    $label = if ($Scenario) { $Scenario } else { "default-$UseAgent" }
    $log = Join-Path $logDir "$label.log"

    # Set in this process and cleared afterwards: the smoke test passes its own
    # environment to the server it spawns, so anything exported here reaches it.
    $saved = @{}
    $vars = $Vars.Clone()
    $vars['SMOKE_AGENT'] = $UseAgent
    $vars['SMOKE_CWD'] = $Workspace
    if ($Scenario) { $vars['SMOKE_ONLY'] = $Scenario }
    if ($UseAgent -ne 'mock') { $vars['SMOKE_PROMPT'] = 'Reply with exactly: SMOKE-OK. Use no tools.' }

    foreach ($k in $vars.Keys) {
        $saved[$k] = [Environment]::GetEnvironmentVariable($k)
        [Environment]::SetEnvironmentVariable($k, $vars[$k])
    }
    try {
        # Redirection happens inside cmd.exe on purpose. Windows PowerShell wraps
        # every stderr line of a native command in an ErrorRecord, which under
        # ErrorActionPreference=Stop terminates the step — so a server that merely
        # logged "restored 100 task(s)" to stderr was reported as a failed
        # scenario while the run itself had passed. Keeping the redirect in cmd
        # means PowerShell never sees the stream, and the exit code stays the only
        # thing that decides pass or fail.
        #
        # The two streams stay in one file because the server's stderr is half the
        # evidence: "interactive approval: http://..." is how you tell an enabled
        # approval endpoint from one that quietly failed to start.
        & cmd.exe /c "go run ./cmd/smoketest ./cli-agent-mcp.exe > `"$log`" 2>&1"
        $code = $LASTEXITCODE
        # -Encoding UTF8 because the transcript is whatever the Go binaries wrote,
        # and Get-Content otherwise decodes it as the ANSI codepage: every arrow,
        # tick and dash in the progress lines comes back as mojibake, in the one
        # output a person reads to work out what went wrong.
        if (Test-Path $log) { Get-Content $log -Encoding UTF8 | Write-Host }
        if ($code -ne 0) { throw "$label failed (exit $code); transcript: $log" }
    } finally {
        foreach ($k in $saved.Keys) { [Environment]::SetEnvironmentVariable($k, $saved[$k]) }
    }
}

$agentAvailable = [bool] (Get-Command $Agent -ErrorAction SilentlyContinue)
if (-not $agentAvailable) {
    Write-Host "warning: '$Agent' is not on PATH; the scenarios that need a real agent will be reported as skipped, not passed." -ForegroundColor Yellow
}

$results = @()
foreach ($step in $suite) {
    if ($Only -and $step.Name -ne $Only) { continue }
    if ($step.Needs -eq 'agent' -and -not $agentAvailable) {
        $results += [pscustomobject]@{ Step = $step.Name; Result = 'SKIPPED'; Detail = "$Agent not on PATH" }
        continue
    }
    Write-Head $step.Name
    try {
        & $step.Script
        $results += [pscustomobject]@{ Step = $step.Name; Result = 'PASS'; Detail = '' }
    } catch {
        # Keep going: knowing that three of four scenarios also fail is worth
        # more than stopping at the first, and every transcript is on disk.
        $results += [pscustomobject]@{ Step = $step.Name; Result = 'FAIL'; Detail = $_.Exception.Message }
    }
}

Write-Head 'Summary'
$results | Format-Table -AutoSize
Write-Host "transcripts: $logDir"

$failed  = @($results | Where-Object Result -eq 'FAIL')
$skipped = @($results | Where-Object Result -eq 'SKIPPED')

if ($failed.Count) {
    Write-Host "$($failed.Count) step(s) FAILED" -ForegroundColor Red
    exit 1
}
if ($skipped.Count) {
    # Not a pass. A skipped real-agent run is the exact hole this script exists
    # to close, so it must not read as a green light.
    Write-Host "no failures, but $($skipped.Count) step(s) were SKIPPED — this is not a release gate pass" -ForegroundColor Yellow
    exit 2
}
Write-Host 'ALL PASSED' -ForegroundColor Green
