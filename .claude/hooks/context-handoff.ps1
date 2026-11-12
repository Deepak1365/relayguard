# Context-size handoff reminder (UserPromptSubmit + PostToolUse).
# Reads the session transcript, takes the context size from the last main-thread
# assistant message's usage (input + cache_creation + cache_read + output), and
# once it reaches the threshold injects an instruction to do the session handoff.
# Fires once at the threshold, then again every +RepeatStep tokens.
# Threshold override: env RELAYGUARD_HANDOFF_TOKENS. Never blocks: any error -> exit 0.
$ErrorActionPreference = 'Stop'
try {
    $Threshold = 140000
    if ($env:RELAYGUARD_HANDOFF_TOKENS) { $Threshold = [int]$env:RELAYGUARD_HANDOFF_TOKENS }
    $RepeatStep = 20000

    $payload = [Console]::In.ReadToEnd() | ConvertFrom-Json
    $path = $payload.transcript_path
    if (-not $path -or -not (Test-Path -LiteralPath $path)) { exit 0 }

    # Read only the tail of the transcript (shared read: Claude Code keeps writing it).
    $fs = [System.IO.File]::Open($path, 'Open', 'Read', 'ReadWrite')
    try {
        $len = $fs.Length
        $take = [Math]::Min($len, 4MB)
        [void]$fs.Seek($len - $take, 'Begin')
        $buf = New-Object byte[] $take
        $read = 0
        while ($read -lt $take) {
            $n = $fs.Read($buf, $read, $take - $read)
            if ($n -le 0) { break }
            $read += $n
        }
    } finally { $fs.Dispose() }
    $lines = [System.Text.Encoding]::UTF8.GetString($buf, 0, $read) -split "`n"

    $tokens = $null
    for ($i = $lines.Count - 1; $i -ge 0; $i--) {
        $l = $lines[$i]
        if ($l -notlike '*"type":"assistant"*' -or $l -notlike '*"usage"*') { continue }
        try { $o = $l | ConvertFrom-Json } catch { continue }
        if ($o.isSidechain) { continue }
        $u = $o.message.usage
        if (-not $u) { continue }
        $tokens = [int64]$u.input_tokens + [int64]$u.cache_creation_input_tokens + [int64]$u.cache_read_input_tokens + [int64]$u.output_tokens
        break
    }
    if ($null -eq $tokens -or $tokens -lt $Threshold) { exit 0 }

    # Once per threshold crossing, then every +RepeatStep tokens (per session).
    $sid = if ($payload.session_id) { $payload.session_id } else { 'unknown' }
    $marker = Join-Path $env:TEMP "relayguard-handoff-$sid.txt"
    if (Test-Path -LiteralPath $marker) {
        $last = [int64](Get-Content -LiteralPath $marker -Raw)
        if ($tokens -lt $last + $RepeatStep) { exit 0 }
    }
    Set-Content -LiteralPath $marker -Value $tokens

    $k = [Math]::Round($tokens / 1000)
    $newPrompt = 'Read CLAUDE.md (especially Status), docs/ROADMAP.md, and git status / git log --oneline -10. Tell me in 5 lines where we are and what the next step is. Don''t run anything else until I confirm.'
    $ctx = @"
CONTEXT HANDOFF DUE: the conversation context is about ${k}k tokens (threshold $([Math]::Round($Threshold / 1000))k).
Finish only the current atomic action (do not start new subagents, builds or experiments; never commit or push without the user's OK). Then do the session handoff:
1. Update the Status section of CLAUDE.md with a short handoff: what was just finished, what is in progress (exact step), any decisions the user made in this conversation that aren't written down anywhere yet, uncommitted files and why, and the exact next step.
2. Tick anything finished in docs/ROADMAP.md.
3. Show the user the diff.
4. End your message with this prompt for the user to paste into a new chat, in a code block:
$newPrompt
"@
    $out = @{
        hookSpecificOutput = @{ hookEventName = $payload.hook_event_name; additionalContext = $ctx }
        systemMessage      = "Context ~${k}k tokens: session handoff due (RelayGuard rule)."
    }
    $out | ConvertTo-Json -Depth 4 -Compress
    exit 0
} catch {
    exit 0
}
