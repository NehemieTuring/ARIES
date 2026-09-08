package claudecodessh

import (
	"errors"
	"regexp"
	"strings"
)

// DESIGN NOTE — revised after empirical capture (rapport_integration_claude_code.md,
// step 2). The original version of this file assumed Claude Code's Bash tool
// kept ONE persistent interactive shell process alive for the whole session
// (so state like `cd` would survive between tool calls at the OS level). That
// assumption was WRONG and has been empirically disproved: shimming /bin/bash
// inside a disposable container and observing real invocations from a live
// `claude -p` run showed Claude Code issues a fresh, discrete `bash -c
// "<script>"` exec for every single tool call — exactly the same
// discrete-exec-per-command model Hermes and OpenClaw already use. This
// bridge's grammar can therefore mirror hermesssh/grammar.go's approach
// instead of the continuous-stream design described in the superseded version
// of this file.
//
// Four distinct shapes were observed for one `claude -p` run with two Bash
// tool calls ("ls -la" then "date"):
//
//  1. `bash -c env`
//     A one-time environment probe at session start.
//
//  2. `bash -c -l SNAPSHOT_FILE=/home/<user>/.claude/shell-snapshots/snapshot-bash-<epoch>-<rand>.sh
//     source "<home>/.bashrc" < /dev/null
//     ...` (a long, fixed-shape script)
//     Exactly once per session: sources the user's .bashrc, captures every
//     function/alias/shell-option/PATH into $SNAPSHOT_FILE, and defines
//     Claude's own rg/find/grep/pkill shadowing functions inside that
//     snapshot. This is Claude Code's mechanism for faking persistent shell
//     state across the discrete execs below — the actual OS process is never
//     kept alive; the snapshot FILE is what persists (in the sandbox's
//     filesystem, across exec channels).
//
//  3. `bash -c "source $SNAPSHOT_FILE 2>/dev/null || true && shopt -u extglob 2>/dev/null || true &&
//     { \builtin unalias -- 'unsetenv'; \builtin unset -f -- 'unsetenv'; } >/dev/null 2>&1 || true &&
//     eval '<command>' < /dev/null && pwd -P >| /tmp/claude-<id>-cwd"`
//     One per actual tool call: re-sources the snapshot (restoring functions/
//     aliases/PATH), evaluates the real command, then records the resulting
//     cwd to a temp file so Claude Code can detect a `cd` and carry it into
//     the next call's context.
//
// Consequently, unlike the superseded design, THIS bridge:
//   - Dispatches one SSH "exec" channel-request per command, like
//     hermesssh/openclawssh (see bridge.go), not one continuous "shell"
//     channel.
//   - Extracts and journals the real inner command (the argument to `eval`)
//     as the structured toolCallRecord payload — the snapshot/pwd-tracking
//     scaffolding around it is bridge plumbing, not agent intent, and is kept
//     only in the raw log for forensic completeness.
//   - Does NOT require pty support — bash -c has never needed a controlling
//     terminal in any of the observed shapes.
//
// TODO(claude-code): the exact snapshot-file path and the cwd-tracking temp
// file name are session-random and were only exercised for a plain,
// no-special-character command (`ls -la`, `date`). The unquoting logic below
// (decodeEvalArgument) has not yet been exercised against a command containing
// an embedded single quote — verify against a real run before trusting it in
// production.

const (
	kindAgent     = "agent"
	kindBootstrap = "bootstrap"
	kindUnknown   = "unknown"
	// kindFileOp identifies a command from the companion MCP file-tools server
	// (cmd/aries-claudecode-mcpfiles), decoded by fileops.go rather than by
	// this file's wrapperPattern — see fileops.go's package doc comment for
	// why that command family needs a separate grammar.
	kindFileOp = "fileop"
)

// wrapperPattern matches shape 3 above and captures the single-quoted `eval`
// argument (group 1) verbatim, still bash-quoted.
//
// The leading `source $SNAPSHOT_FILE ... &&` clause is OPTIONAL: the first
// empirical capture (§7.2, via a shimmed host-local su/run.sh setup) always
// had it, but a later end-to-end run through the real bridge (§7.6) showed
// real per-call commands sometimes omitting it entirely — observed as
// `bash -c -l "shopt -u extglob ... && eval '<cmd>' ... && pwd -P >| ..."`
// with no `source` clause at all. The exact condition under which Claude Code
// includes vs. omits it is not yet understood (possibly whether the `-l` flag
// itself already covers sourcing shell state, making the explicit `source`
// redundant) — both shapes are accepted rather than guessing further.
var wrapperPattern = regexp.MustCompile(
	`^(?:source .*? 2>/dev/null \|\| true && )?shopt -u extglob 2>/dev/null \|\| true && \{ \\builtin unalias -- 'unsetenv'; \\builtin unset -f -- 'unsetenv'; \} >/dev/null 2>&1 \|\| true && eval (.*) < /dev/null && pwd -P >\| .*$`,
)

// snapshotBootstrapPattern matches shape 2 (the once-per-session snapshot
// generator). It is not further decoded — only classified — since it carries
// no agent-chosen command, just Claude Code's own fixed scaffolding script.
var snapshotBootstrapMarker = "# Snapshot file"

type remoteCommand struct {
	// script is the real, agent-chosen command (unquoted), populated only for
	// kindAgent.
	script string
	kind   string
}

// decodeRemoteCommand classifies and, for an agent command, extracts the real
// command from one bash invocation. The real observed argv for the
// once-per-session snapshot generator is THREE elements — `["-c", "-l",
// "<script>"]`, not just `["-l", "<script>"]` as first assumed (see
// rapport_integration_claude_code.md §7.6: caught by an end-to-end run
// rejecting every real Bash tool call, all four retries hashing identical
// because Claude Code was resending the same misclassified snapshot-generator
// script). Classification therefore checks the bootstrap markers on the
// resolved script FIRST, regardless of exactly which flags precede it, before
// ever attempting the stricter per-call wrapperPattern match.
func decodeRemoteCommand(argv []string) (remoteCommand, error) {
	if len(argv) < 2 || argv[0] != "-c" {
		return remoteCommand{}, errors.New("Claude Code exec command must invoke bash with -c")
	}
	script := argv[len(argv)-1]
	if script == "env" {
		return remoteCommand{kind: kindBootstrap}, nil
	}
	if strings.Contains(script, snapshotBootstrapMarker) || strings.Contains(script, "SNAPSHOT_FILE=") {
		return remoteCommand{kind: kindBootstrap}, nil
	}
	matches := wrapperPattern.FindStringSubmatch(script)
	if matches == nil {
		return remoteCommand{}, errors.New("Claude Code exec script does not match the expected per-call wrapper shape")
	}
	inner, err := decodeEvalArgument(matches[1])
	if err != nil {
		return remoteCommand{}, err
	}
	return remoteCommand{script: inner, kind: kindAgent}, nil
}

// decodeEvalArgument un-quotes the single shell word bash produced for `eval
// <word> < /dev/null`.
//
// BUG FOUND AND FIXED (rapport_integration_claude_code.md §9.8): the
// original version handled only ONE way of embedding a literal single quote
// inside a single-quoted string (close, backslash-escaped quote, reopen:
// `'\”`) and required the whole word to start with `'`. A real end-to-end
// run with a command containing an embedded single quote
// (`printf 'Hello ARIES' > greeting.txt && ...`) showed Claude Code's own
// shell-quoting instead produces `'"'"'` (close single quote, open double
// quote containing a literal `'`, close double quote, reopen single quote)
// — a different, equally-valid POSIX idiom for the same thing — and a
// SEPARATE real call passed a bare, entirely unquoted word (`eval pwd`),
// which the old code also rejected outright. Both were observed being
// rejected in a real tool-calls.jsonl/ssh_raw.log before this fix.
//
// Rather than pattern-match specific idioms, this is a general POSIX
// "shell word" unquoter: single-quoted spans are copied verbatim (no
// escapes, per POSIX); double-quoted spans honor backslash only before
// \, $, `, "; outside any quote, backslash escapes exactly the next
// character; anything else outside a quote is copied as-is. Adjacent
// spans (quoted or not) concatenate with no separator, exactly as bash
// itself would join them — which is how both idioms above, and a fully
// unquoted word, all fall out of the same small state machine.
func decodeEvalArgument(word string) (string, error) {
	var value strings.Builder
	position := 0
	for position < len(word) {
		switch word[position] {
		case '\'':
			end := strings.IndexByte(word[position+1:], '\'')
			if end == -1 {
				return "", errors.New("Claude Code eval argument has an unterminated single quote")
			}
			value.WriteString(word[position+1 : position+1+end])
			position += end + 2
		case '"':
			position++
			for position < len(word) && word[position] != '"' {
				if word[position] == '\\' && position+1 < len(word) && strings.IndexByte(`\$`+"`\"", word[position+1]) != -1 {
					value.WriteByte(word[position+1])
					position += 2
					continue
				}
				value.WriteByte(word[position])
				position++
			}
			if position >= len(word) {
				return "", errors.New("Claude Code eval argument has an unterminated double quote")
			}
			position++
		case '\\':
			if position+1 >= len(word) {
				return "", errors.New("Claude Code eval argument ends with a trailing backslash")
			}
			value.WriteByte(word[position+1])
			position += 2
		default:
			value.WriteByte(word[position])
			position++
		}
	}
	return value.String(), nil
}
