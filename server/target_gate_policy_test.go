package server

import "testing"

func TestTargetGatePolicyRequiresLiteralOne(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"", false},
		{"0", false},
		{"true", false},
		{"yes", false},
		{"1", true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv(RequireTargetGateEnv, tc.value)
			if got := targetGateRequiredFromEnv(); got != tc.want {
				t.Fatalf("targetGateRequiredFromEnv() = %v, want %v for %q", got, tc.want, tc.value)
			}
		})
	}
}
