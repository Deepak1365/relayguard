# PreToolUse hook for Bash: block any command that refers to a .env file.
# .env holds the Stripe webhook secret (whsec_...) and must never be read,
# printed, copied or committed by an agent. .env.example is allowed.
#
# Input: the hook JSON on stdin (tool_input.command is the Bash command).
# Output: exit 0 to allow; exit 2 with a reason on stderr to block.
# Fails closed: if the input cannot be parsed, the command is blocked.
#
# This is a tripwire, not a sandbox: an obfuscated reference (globs, string
# building, base64) is not caught. The Read/Edit deny rules in settings.json
# cover the file tools.

try {
    $raw = [Console]::In.ReadToEnd()
    $data = $raw | ConvertFrom-Json
    $cmd = [string]$data.tool_input.command
} catch {
    [Console]::Error.WriteLine("Blocked by .claude/hooks/block-env.ps1: could not parse the hook input, so the command was blocked to be safe.")
    exit 2
}

# Remove the allowed name first, then look for any remaining .env reference:
# .env, .env.local, path/.env, ".env", etc. (.envrc and $env:VAR do not match).
$scrubbed = $cmd -replace '(?i)\.env\.example', ''
if ($scrubbed -match '(?i)\.env(?![A-Za-z0-9_])') {
    [Console]::Error.WriteLine("Blocked by .claude/hooks/block-env.ps1: this command refers to a .env file, which holds the webhook secret. Agents must never read, print, copy or commit .env (CLAUDE.md rule 2). .env.example is allowed. For DB tests use the non-secret dev DATABASE_URL in CLAUDE.md.")
    exit 2
}
exit 0
