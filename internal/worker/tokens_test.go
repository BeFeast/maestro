package worker

import (
	"testing"
)

func TestParseTokensFromOutput(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int
	}{
		{
			name:  "openclaw format",
			input: "tokens 12345 (in 1000 / out 11345)",
			want:  12345,
		},
		{
			name:  "openclaw format no parens",
			input: "tokens 999",
			want:  999,
		},
		{
			name:  "codex-style input+output",
			input: "Tokens: 1234 input, 5678 output",
			want:  6912,
		},
		{
			name:  "codex-style with commas",
			input: "Tokens: 1,234 input, 5,678 output",
			want:  6912,
		},
		{
			name:  "generic total tokens colon",
			input: "total tokens: 42000",
			want:  42000,
		},
		{
			name:  "generic total_tokens underscore",
			input: "total_tokens: 99",
			want:  99,
		},
		{
			name:  "no match",
			input: "nothing to see here",
			want:  0,
		},
		{
			name:  "empty string",
			input: "",
			want:  0,
		},
		{
			name: "multiline — picks max",
			input: `
tokens 100 (in 10 / out 90)
some other line
tokens 5000 (in 1000 / out 4000)
tokens 3000 (in 500 / out 2500)
`,
			want: 5000,
		},
		{
			name:  "tokens with commas",
			input: "tokens 1,234,567 (in 100,000 / out 1,134,567)",
			want:  1234567,
		},
		{
			name:  "case insensitive",
			input: "TOKENS 777 (IN 300 / OUT 477)",
			want:  777,
		},
		{
			name: "codex two-line tokens used (comma-grouped)",
			input: `running step 5
tokens used
89,655
done`,
			want: 89655,
		},
		{
			name: "codex two-line tokens used (small number, no commas)",
			input: `tokens used
512`,
			want: 512,
		},
		{
			name: "codex two-line with blank line between keyword and number",
			input: `tokens used

12,345`,
			want: 12345,
		},
		{
			name: "codex two-line is case insensitive",
			input: `TOKENS USED
2,048`,
			want: 2048,
		},
		{
			name: "codex two-line keyword with log prefix",
			input: `[2026-06-02T10:00:00Z] tokens used
1,000,000`,
			want: 1000000,
		},
		{
			name: "codex two-line ignored when next non-empty line is not a number",
			input: `tokens used
not a number
99,999`,
			want: 0,
		},
		{
			name:  "claude json usage object",
			input: `{"type":"result","usage":{"input_tokens":1200,"output_tokens":3400},"result":"done"}`,
			want:  4600,
		},
		{
			name: "claude stream-json — last result frame wins",
			input: `{"type":"assistant","message":{"usage":{"input_tokens":100,"output_tokens":200}}}
{"type":"assistant","message":{"usage":{"input_tokens":50,"output_tokens":75}}}
{"type":"result","usage":{"input_tokens":150,"output_tokens":275}}`,
			want: 425,
		},
		{
			name: "multi-format log — parser returns the max",
			input: `tokens 100 (in 10 / out 90)
tokens used
89,655
{"type":"result","usage":{"input_tokens":10,"output_tokens":20}}`,
			want: 89655,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseTokensFromOutput(tc.input)
			if got != tc.want {
				t.Errorf("ParseTokensFromOutput(%q) = %d, want %d", tc.input, got, tc.want)
			}
		})
	}
}

func TestFormatTokens(t *testing.T) {
	tests := []struct {
		n    int
		want string
	}{
		{0, "-"},
		{-1, "-"},
		{1, "1"},
		{999, "999"},
		{1000, "1k"},
		{1500, "1.5k"},
		{12300, "12.3k"},
		{999999, "1000k"},
		{1000000, "1M"},
		{1200000, "1.2M"},
		{10000000, "10M"},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			got := FormatTokens(tc.n)
			if got != tc.want {
				t.Errorf("FormatTokens(%d) = %q, want %q", tc.n, got, tc.want)
			}
		})
	}
}
