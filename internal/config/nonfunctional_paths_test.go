package config

import (
	"reflect"
	"testing"
)

func TestEffectiveNonFunctionalPaths(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "default is docs only",
			in:   nil,
			want: []string{"docs/**"},
		},
		{
			name: "project extensions union with default, docs first",
			in:   []string{"qa/**", "records/**"},
			want: []string{"docs/**", "qa/**", "records/**"},
		},
		{
			name: "blank and duplicate entries are dropped",
			in:   []string{"  ", "docs/**", "qa/**", "qa/**"},
			want: []string{"docs/**", "qa/**"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SupervisorConfig{NonFunctionalPaths: tc.in}.EffectiveNonFunctionalPaths()
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("EffectiveNonFunctionalPaths() = %v, want %v", got, tc.want)
			}
		})
	}
}
