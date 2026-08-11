package github

import "testing"

// #1168: an explicit leading [Px] marker is authoritative — the substring
// needles must not promote an advisory whose message merely quotes "P0/P1".
func TestHasSeverityMarker_ExplicitMarkerPrecedence(t *testing.T) {
	cases := []struct {
		name string
		body string
		high bool
		crit bool
	}{
		{
			name: "advisory quoting the severity contract stays advisory",
			body: "[P3] consider documenting which findings are P0/P1 blockers\n\n<sub>llm-review-opus @ abcdef123456</sub>",
			high: false,
			crit: false,
		},
		{
			name: "P1 with a P0 quote in the message is high but not critical",
			body: "[P1] this mirrors the P0 class from the contract",
			high: true,
			crit: false,
		},
		{
			name: "plain P0 finding",
			body: "[P0] data loss on restart",
			high: true,
			crit: true,
		},
		{
			name: "indented marker still explicit",
			body: "  [p2] lowercase and indented",
			high: false,
			crit: false,
		},
		{
			name: "greptile badge without leading marker keeps needle behavior",
			body: `<img alt="P1" src="https://img.shields.io/badge/P1-red"> nil deref`,
			high: true,
			crit: false,
		},
		{
			name: "prose severity label without leading marker keeps needle behavior",
			body: "severity: P0 — guaranteed breakage",
			high: true,
			crit: true,
		},
		{
			name: "mid-body bracket marker is not a leading marker",
			body: "see the [P0] note above; this is informational",
			high: true, // needle behavior preserved: no LEADING marker present
			crit: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isHighSeverity(tc.body); got != tc.high {
				t.Fatalf("isHighSeverity(%q) = %v, want %v", tc.body, got, tc.high)
			}
			if got := isCriticalSeverity(tc.body); got != tc.crit {
				t.Fatalf("isCriticalSeverity(%q) = %v, want %v", tc.body, got, tc.crit)
			}
		})
	}
}
