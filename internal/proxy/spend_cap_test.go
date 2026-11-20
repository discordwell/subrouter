package proxy

import "testing"

func TestSpendCapClassification(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"workspace code", `{"error":{"code":"workspace_usage_limit_reached"}}`, true},
		{"spend cap", `{"error":{"message":"You hit your spend cap set by the owner of your workspace."}}`, true},
		{"normal output", `{"type":"response.output_text.delta","delta":"You hit your spend cap"}`, false},
		{"unrelated forbidden", `{"error":{"message":"Access denied"}}`, false},
		{"cap configuration", `{"error":{"message":"Unable to configure spend cap"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := usageLimitJSON([]byte(tc.body)); got != tc.want {
				t.Fatalf("quota=%v, want %v", got, tc.want)
			}
		})
	}
}
