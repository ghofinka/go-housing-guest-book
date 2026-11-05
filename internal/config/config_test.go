package config

import (
	"strings"
	"testing"
)

// clearSMTP empties every SMTP_* variable for the test (t.Setenv restores them afterwards).
func clearSMTP(t *testing.T) {
	t.Helper()
	for _, k := range []string{"SMTP_HOST", "SMTP_PORT", "SMTP_USERNAME", "SMTP_PASSWORD", "SMTP_FROM"} {
		t.Setenv(k, "")
	}
}

func TestLoadSMTP(t *testing.T) {
	const secret = "s3cret-password-value"
	cases := []struct {
		name    string
		env     map[string]string
		wantErr string // substring, "" for success
		enabled bool
		port    int
	}{
		{name: "nothing set means off", env: nil},
		{name: "host and sender", env: map[string]string{"SMTP_HOST": "mail.example.test", "SMTP_FROM": "Buku Tamu <noreply@example.test>"}, enabled: true, port: 587},
		{name: "custom port", env: map[string]string{"SMTP_HOST": "mail.example.test", "SMTP_FROM": "noreply@example.test", "SMTP_PORT": "465"}, enabled: true, port: 465},
		{name: "with login", env: map[string]string{"SMTP_HOST": "mail.example.test", "SMTP_FROM": "noreply@example.test", "SMTP_USERNAME": "u", "SMTP_PASSWORD": secret}, enabled: true, port: 587},
		{name: "port without host", env: map[string]string{"SMTP_PORT": "587"}, wantErr: "SMTP_HOST is required"},
		{name: "sender without host", env: map[string]string{"SMTP_FROM": "noreply@example.test"}, wantErr: "SMTP_HOST is required"},
		{name: "password without host", env: map[string]string{"SMTP_PASSWORD": secret}, wantErr: "SMTP_HOST is required"},
		{name: "host without sender", env: map[string]string{"SMTP_HOST": "mail.example.test"}, wantErr: "SMTP_FROM is required"},
		{name: "bad sender", env: map[string]string{"SMTP_HOST": "mail.example.test", "SMTP_FROM": "not an address"}, wantErr: "SMTP_FROM is not a valid"},
		{name: "bad port", env: map[string]string{"SMTP_HOST": "mail.example.test", "SMTP_FROM": "a@example.test", "SMTP_PORT": "abc"}, wantErr: "SMTP_PORT"},
		{name: "port out of range", env: map[string]string{"SMTP_HOST": "mail.example.test", "SMTP_FROM": "a@example.test", "SMTP_PORT": "70000"}, wantErr: "SMTP_PORT"},
		{name: "username without password", env: map[string]string{"SMTP_HOST": "mail.example.test", "SMTP_FROM": "a@example.test", "SMTP_USERNAME": "u"}, wantErr: "must be set together"},
		{name: "password without username", env: map[string]string{"SMTP_HOST": "mail.example.test", "SMTP_FROM": "a@example.test", "SMTP_PASSWORD": secret}, wantErr: "must be set together"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearSMTP(t)
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			got, err := loadSMTP()
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, c.wantErr)
				}
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("the error leaks the password: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Enabled() != c.enabled || got.Port != c.port {
				t.Fatalf("got enabled=%v port=%d, want enabled=%v port=%d", got.Enabled(), got.Port, c.enabled, c.port)
			}
		})
	}
}
