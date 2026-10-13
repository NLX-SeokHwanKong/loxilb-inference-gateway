package loxinet

import "testing"

func TestCanonicalLBPortMax(t *testing.T) {
	for _, tt := range []struct {
		name            string
		port, max, want uint16
	}{
		{"omitted", 2210, 0, 0},
		{"explicit singleton", 2210, 2210, 0},
		{"lowest singleton", 1, 1, 0},
		{"highest singleton", 65535, 65535, 0},
		{"wildcard singleton", 0, 0, 0},
		{"real range", 2210, 2212, 2212},
		{"wildcard range", 0, 2212, 2212},
		{"inverted still rejected downstream", 2210, 2209, 2209},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := canonicalLBPortMax(tt.port, tt.max); got != tt.want {
				t.Fatalf("port=%d max=%d: got %d, want %d", tt.port, tt.max, got, tt.want)
			}
		})
	}
}
