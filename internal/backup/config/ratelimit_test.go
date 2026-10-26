package config

import (
	"math"
	"testing"
)

func TestResolveRateLimit(t *testing.T) {
	t.Parallel()

	defaults := RateLimitSettings{
		RequestsPerSecond:     DefaultRateLimitRequestsPerSecond,
		Burst:                 DefaultRateLimitBurst,
		AuthFailuresPerMinute: DefaultRateLimitAuthFailuresPerMinute,
		AuthFailureBurst:      DefaultRateLimitAuthFailureBurst,
	}

	tests := []struct {
		name    string
		input   fileWebUIRateLimit
		want    RateLimitSettings
		wantErr bool
	}{
		{name: "unset takes the defaults", want: defaults},
		{
			name:  "set values are kept",
			input: fileWebUIRateLimit{RequestsPerSecond: 2.5, Burst: 10, AuthFailuresPerMinute: 3, AuthFailureBurst: 4},
			want:  RateLimitSettings{RequestsPerSecond: 2.5, Burst: 10, AuthFailuresPerMinute: 3, AuthFailureBurst: 4},
		},
		{
			name:  "disabled keeps the defaults for the rest",
			input: fileWebUIRateLimit{Disabled: true},
			want: RateLimitSettings{
				Disabled: true, RequestsPerSecond: defaults.RequestsPerSecond, Burst: defaults.Burst,
				AuthFailuresPerMinute: defaults.AuthFailuresPerMinute, AuthFailureBurst: defaults.AuthFailureBurst,
			},
		},
		{name: "negative rate rejected", input: fileWebUIRateLimit{RequestsPerSecond: -1}, wantErr: true},
		{name: "negative burst rejected", input: fileWebUIRateLimit{AuthFailureBurst: -1}, wantErr: true},
		{name: "infinite rate rejected", input: fileWebUIRateLimit{AuthFailuresPerMinute: math.Inf(1)}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := resolveRateLimit(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveRateLimit(%+v) = %+v, want an error", tt.input, got)
				}

				return
			}

			if err != nil {
				t.Fatalf("resolveRateLimit(%+v) unexpected error: %v", tt.input, err)
			}

			if got != tt.want {
				t.Errorf("resolveRateLimit(%+v) = %+v, want %+v", tt.input, got, tt.want)
			}
		})
	}
}
