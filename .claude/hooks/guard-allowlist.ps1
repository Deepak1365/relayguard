# PreToolUse hook for Bash: some allow rules in settings.json are prefixes
# (git diff/log/show, go test) whose commands are read-only or test-only,
# EXCEPT for flags that write files to an arbitrary path. When such a flag
# appears, this hook downgrades the command from "allow" to "ask" so the user
# sees it. It never blocks outright.
#
# Flags caught:
#   git diff|log|show ... --output[=]<path>     (writes the output to a file)
#   go test|build ... -o <path>, -c, -*profile  (writes a binary or profile)
#
# Input: the hook JSON on stdin (tool_input.command).
# Output: exit 0 and no output to leave the decision to settings.json;
# exit 0 with a permissionDecision "ask" JSON to force a prompt.
# Fails toward asking: if the input cannot be parsed, it asks.

function Ask([string]$reason) {
    $out = @{
        hookSpecificOutput = @{
            hookEventName            = "PreToolUse"
            permissionDecision       = "ask"
            permissionDecisionReason = $reason
        }
    } | ConvertTo-Json -Compress -Depth 4
    [Console]::Out.WriteLine($out)
    exit 0
}

try {
    $raw = [Console]::In.ReadToEnd()
    $data = $raw | ConvertFrom-Json
    $cmd = [string]$data.tool_input.command
} catch {
    Ask "guard-allowlist.ps1 could not parse the hook input, so it asks to be safe."
}

if ($cmd -match '(?i)\bgit\s+(diff|log|show)\b.*\s--output\b') {
    Ask "git --output writes a file (possibly outside the repo); the allow rule only covers read-only use."
}
if ($cmd -match '(?i)\bgo\s+(test|build)\b.*(\s-o(\s|=)|\s-c(\s|$)|\s-\w*profile(\s|=))') {
    Ask "go test/build with -o, -c or a -*profile flag writes a file to a chosen path; the allow rule only covers plain builds and tests."
}
exit 0
