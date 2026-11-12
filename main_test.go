package main

import "testing"

func TestRunHelpReturnsZero(t *testing.T) {
	if code := run([]string{"--help"}); code != 0 {
		t.Fatalf("--help should exit successfully, got %d", code)
	}
}

func TestRunDoesNotExposeProFlags(t *testing.T) {
	for _, arg := range []string{"--auto-cordon", "--auto-drain", "--webhook-url", "--license-key"} {
		t.Run(arg, func(t *testing.T) {
			if code := run([]string{arg}); code != 2 {
				t.Fatalf("removed Pro option %q should be rejected, got exit code %d", arg, code)
			}
		})
	}
}

func TestColorStatus(t *testing.T) {
	cases := []struct {
		status string
		color  string
	}{
		{"CRITICAL: RAID DEGRADED", colorRed},
		{"WARNING: RAID RESYNCING", colorYellow},
		{"UNKNOWN (CPU sensor unavailable)", colorYellow},
		{"HEALTHY", colorGreen},
	}
	for _, tc := range cases {
		if got := colorStatus(tc.status); len(got) < len(tc.color) || got[:len(tc.color)] != tc.color {
			t.Errorf("colorStatus(%q) should start with %q, got %q", tc.status, tc.color, got)
		}
	}
}
