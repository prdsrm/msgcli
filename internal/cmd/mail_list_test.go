package cmd

import "testing"

func TestParseDateArg(t *testing.T) {
	cases := map[string]string{
		"2026-04-20":           "2026-04-20T00:00:00Z",
		"2026-04-20T22:35:35Z": "2026-04-20T22:35:35Z",
	}
	for in, want := range cases {
		got, err := parseDateArg(in)
		if err != nil {
			t.Errorf("parseDateArg(%q) failed: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseDateArg(%q) = %q, want %q", in, got, want)
		}
	}

	for _, bad := range []string{"20/04/2026", "yesterday", ""} {
		if _, err := parseDateArg(bad); err == nil {
			t.Errorf("parseDateArg(%q) should fail", bad)
		}
	}
}
