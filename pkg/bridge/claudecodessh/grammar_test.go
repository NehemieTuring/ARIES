package claudecodessh

import "testing"

func TestDecodeRemoteCommandKeepsSnapshotTextInsideAgentCommand(t *testing.T) {
	script := `shopt -u extglob 2>/dev/null || true && { \builtin unalias -- 'unsetenv'; \builtin unset -f -- 'unsetenv'; } >/dev/null 2>&1 || true && eval 'grep -r SNAPSHOT_FILE= .' < /dev/null && pwd -P >| /tmp/claude-cwd`
	got, err := decodeRemoteCommand([]string{"-c", script})
	if err != nil || got.kind != kindAgent || got.script != "grep -r SNAPSHOT_FILE= ." {
		t.Fatalf("decode = %#v, %v", got, err)
	}
}

func TestDecodeRemoteCommandClassifiesSnapshotGenerator(t *testing.T) {
	script := "SNAPSHOT_FILE=/home/aries/.claude/shell-snapshots/snapshot-bash-1.sh\n# Snapshot file\n"
	got, err := decodeRemoteCommand([]string{"-c", "-l", script})
	if err != nil || got.kind != kindBootstrap || got.script != "" {
		t.Fatalf("decode = %#v, %v", got, err)
	}
	if _, err := decodeRemoteCommand([]string{"-c", script}); err == nil {
		t.Fatal("snapshot text without -l was accepted")
	}
}

// These three cases are the exact shapes observed in a real end-to-end run,
// captured from tool-calls.jsonl and ssh_raw.log after decodeEvalArgument
// rejected them.
func TestDecodeEvalArgument(t *testing.T) {
	cases := []struct {
		name    string
		word    string
		want    string
		wantErr bool
	}{
		{
			name: "bare unquoted word",
			word: "pwd",
			want: "pwd",
		},
		{
			name: "simple single-quoted command",
			word: `'ls -la'`,
			want: "ls -la",
		},
		{
			name: "embedded single quote via close/backslash-quote/reopen",
			word: `'echo '\''hi'\'''`,
			want: `echo 'hi'`,
		},
		{
			name: "embedded single quotes via close/double-quote/reopen (real capture)",
			word: `'printf '"'"'Hello ARIES'"'"' > greeting.txt && sed -i '"'"'s/Hello/Hi/'"'"' greeting.txt && cat greeting.txt'`,
			want: `printf 'Hello ARIES' > greeting.txt && sed -i 's/Hello/Hi/' greeting.txt && cat greeting.txt`,
		},
		{
			name: "empty single-quoted command",
			word: `''`,
			want: "",
		},
		{
			name:    "unterminated single quote",
			word:    `'ls -la`,
			wantErr: true,
		},
		{
			name:    "unterminated double quote",
			word:    `"'`,
			wantErr: true,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := decodeEvalArgument(testCase.word)
			if testCase.wantErr {
				if err == nil {
					t.Fatalf("decodeEvalArgument(%q) = %q, nil; want an error", testCase.word, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("decodeEvalArgument(%q) returned error: %v", testCase.word, err)
			}
			if got != testCase.want {
				t.Fatalf("decodeEvalArgument(%q) = %q, want %q", testCase.word, got, testCase.want)
			}
		})
	}
}
