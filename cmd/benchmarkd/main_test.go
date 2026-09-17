package main

import "testing"

// env builds a getenv func from a map: the empty string means "unset".
func env(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

func TestResolveAddr(t *testing.T) {
	tests := []struct {
		name string
		vars map[string]string
		want string
	}{
		{"nothing set", nil, defaultListenAddr},
		{"empty values", map[string]string{"BENCHMARK_ADDR": "", "SERVICE_PORT": ""}, defaultListenAddr},
		{"blank SERVICE_PORT", map[string]string{"SERVICE_PORT": "   "}, defaultListenAddr},
		{"SERVICE_PORT as a port", map[string]string{"SERVICE_PORT": "8080"}, "127.0.0.1:8080"},
		{"SERVICE_PORT with padding", map[string]string{"SERVICE_PORT": " 8080 "}, "127.0.0.1:8080"},
		{"SERVICE_PORT as host:port", map[string]string{"SERVICE_PORT": "0.0.0.0:8080"}, "0.0.0.0:8080"},
		{"SERVICE_PORT without a host", map[string]string{"SERVICE_PORT": ":8080"}, "127.0.0.1:8080"},
		{"BENCHMARK_ADDR wins", map[string]string{"BENCHMARK_ADDR": "127.0.0.1:9000", "SERVICE_PORT": "8080"}, "127.0.0.1:9000"},
		{"not a port", map[string]string{"SERVICE_PORT": "http"}, defaultListenAddr},
		{"port 0 is not a real port", map[string]string{"SERVICE_PORT": "0"}, defaultListenAddr},
		{"port out of range", map[string]string{"SERVICE_PORT": "70000"}, defaultListenAddr},
		{"negative port", map[string]string{"SERVICE_PORT": "-1"}, defaultListenAddr},
		{"ambient PORT/HOST are ignored", map[string]string{"PORT": "9999", "HOST": "0.0.0.0"}, defaultListenAddr},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveAddr(env(tc.vars)); got != tc.want {
				t.Fatalf("resolveAddr=%q, want %q", got, tc.want)
			}
		})
	}
}

// The real process environment has to reach resolveAddr, not just the unit test.
func TestDefaultAddrReadsProcessEnv(t *testing.T) {
	t.Setenv("BENCHMARK_ADDR", "")
	t.Setenv("SERVICE_PORT", "8123")
	if got := defaultAddr(); got != "127.0.0.1:8123" {
		t.Fatalf("defaultAddr=%q, want 127.0.0.1:8123 from SERVICE_PORT", got)
	}
	t.Setenv("SERVICE_PORT", "")
	if got := defaultAddr(); got != defaultListenAddr {
		t.Fatalf("defaultAddr=%q, want the default %q", got, defaultListenAddr)
	}
}
