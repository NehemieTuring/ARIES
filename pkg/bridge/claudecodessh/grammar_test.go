package claudecodessh

import "testing"

// These three cases are the exact shapes observed in a real end-to-end run
// (rapport_integration_claude_code.md §9.8), captured from tool-calls.jsonl/
// ssh_raw.log after decodeEvalArgument rejected them — not hypothetical.
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
