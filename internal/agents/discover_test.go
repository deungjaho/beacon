package agents

import "testing"

func TestIsSafeSessionID(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"019da80e-bc54-7a60-88b1-f3614832ca5a", true},
		{"abc123", true},
		{"ABC-123", true},
		{"", false},
		{"'; DROP TABLE sessions; --", false},
		{"$(rm -rf /)", false},
		{string(make([]byte, 65)), false}, // too long
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			if got := isSafeSessionID(tt.id); got != tt.want {
				t.Errorf("isSafeSessionID(%q) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
}

func TestIsSafeSessionIDRejectsSQLInjection(t *testing.T) {
	// Common SQL injection patterns must be rejected
	evil := []string{
		"' OR '1'='1",
		"'; DROP TABLE sessions; --",
		"1' UNION SELECT 1--",
		"1; DELETE FROM sessions WHERE 1=1",
	}
	for _, e := range evil {
		if isSafeSessionID(e) {
			t.Errorf("isSafeSessionID(%q) should reject SQL injection", e)
		}
	}
}
