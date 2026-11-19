package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidate(t *testing.T) {
	cases := []struct {
		host, password string
		allow, ok      bool
	}{
		{"127.0.0.1", "", false, true},
		{"localhost", "", false, true},
		{"::1", "", false, true},
		{"0.0.0.0", "", false, false},
		{"0.0.0.0", "secret", false, true},
		{"0.0.0.0", "", true, true},
	}
	for _, c := range cases {
		cfg := Config{Host: c.host, Password: c.password, AllowNoPassword: c.allow}
		if err := cfg.Validate(); (err == nil) != c.ok {
			t.Errorf("Validate(host=%q, password=%q, allow=%v) = %v", c.host, c.password, c.allow, err)
		}
	}
}

func TestFromEnvDefaults(t *testing.T) {
	for _, k := range []string{"HOST", "PORT", "PUBLIC_URL", "OAUTH_REDIRECT_URL", "DATA_DIR", "YOUTUBE_DAILY_QUOTA",
		"AI_BASE_URL", "OPENAI_BASE_URL", "AI_MODEL", "OPENAI_MODEL"} {
		t.Setenv(k, "")
	}
	t.Setenv("PORT", "9090")
	t.Setenv("OPENAI_MODEL", "legacy-model") // old names still work

	c := FromEnv()
	if c.Addr() != "127.0.0.1:9090" {
		t.Errorf("Addr = %q", c.Addr())
	}
	if c.Google.RedirectURL != "http://localhost:9090/auth/callback" {
		t.Errorf("RedirectURL = %q", c.Google.RedirectURL)
	}
	if c.AI.Model != "legacy-model" || c.AI.BaseURL != "https://api.openai.com/v1" {
		t.Errorf("AI = %+v", c.AI)
	}
	if c.YouTubeDailyQuota != 10_000 || c.DataDir != "data" {
		t.Errorf("quota %d, data dir %q", c.YouTubeDailyQuota, c.DataDir)
	}
}

func TestLoadDotEnvDoesNotOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(path, []byte("# comment\nYT_TEST_A=\"from file\"\nYT_TEST_B=from file\n"), 0o600)
	t.Setenv("YT_TEST_B", "from env")
	os.Unsetenv("YT_TEST_A")
	t.Cleanup(func() { os.Unsetenv("YT_TEST_A") })

	LoadDotEnv(path)
	if got := os.Getenv("YT_TEST_A"); got != "from file" {
		t.Errorf("YT_TEST_A = %q", got)
	}
	if got := os.Getenv("YT_TEST_B"); got != "from env" {
		t.Errorf("YT_TEST_B = %q, the real environment must win", got)
	}
}
