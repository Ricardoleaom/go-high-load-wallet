package ids

import "testing"

func TestValid(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1", true},
		{"0192F28F-5DC0-7D58-BDB2-814AD6A0F4A1", true},
		{"", false},
		{"abc", false},
		{"0192f28f5dc07d58bdb2814ad6a0f4a1", false},
		{"urn:uuid:0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1", false},
		{"{0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1}", false},
		{"0192f28f-5dc0-7d58-bdb2-814ad6a0f4ag", false},
	} {
		if got := Valid(tc.id); got != tc.want {
			t.Errorf("Valid(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}
