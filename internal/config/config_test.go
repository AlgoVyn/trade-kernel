package config

import (
	"os"
	"strings"
	"testing"
)

func TestValidateRejectsNegativeLimits(t *testing.T) {
	cases := []struct {
		name  string
		patch func(*Config)
		want  string
	}{
		{"negative max_order_qty", func(c *Config) { c.Limits.MaxOrderQty = -1 }, "max_order_qty"},
		{"negative max_position_qty", func(c *Config) { c.Limits.MaxPositionQty = -1 }, "max_position_qty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Config{APIKeyID: "k", APISecretKey: "s", Paper: true}
			tc.patch(&c)
			err := c.Validate()
			if err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestValidateZeroLimitsAllowed(t *testing.T) {
	// Zero means "disabled" and must be accepted (not treated as negative).
	c := Config{
		APIKeyID: "k", APISecretKey: "s", Paper: true,
		Limits: Limits{MaxOrderQty: 0, MaxPositionQty: 0},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("zero limits should validate: %v", err)
	}
}

func TestSMAPeriodAliasForEMA2(t *testing.T) {
	tmp := t.TempDir() + "/tk.yaml"
	if err := os.WriteFile(tmp, []byte(`
api_key_id: k
api_secret_key: s
paper: true
indicators:
  ema_period: 9
  sma_period: 21
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("APCA_API_KEY_ID", "")
	t.Setenv("APCA_API_SECRET_KEY", "")
	c, err := Load(tmp)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Indicators.EMA2Period != 21 {
		t.Fatalf("EMA2Period = %d, want 21 from deprecated sma_period", c.Indicators.EMA2Period)
	}
}

func TestTradeKernelLiveEnvOverride(t *testing.T) {
	// Write a minimal paper config to a temp file.
	tmp := t.TempDir() + "/tk.yaml"
	if err := os.WriteFile(tmp, []byte("paper: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("APCA_API_KEY_ID", "k")
	t.Setenv("APCA_API_SECRET_KEY", "s")

	t.Run("LIVE=1 forces live but needs acknowledgement", func(t *testing.T) {
		t.Setenv("TRADE_KERNEL_LIVE", "1")
		_, err := Load(tmp)
		if err == nil {
			t.Fatal("expected error: live without acknowledgement")
		}
	})
	t.Run("LIVE=0 forces paper", func(t *testing.T) {
		// File says live+acknowledged, env forces paper.
		if err := os.WriteFile(tmp, []byte("paper: false\nlive_trading_acknowledged: true\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("TRADE_KERNEL_LIVE", "0")
		c, err := Load(tmp)
		if err != nil {
			t.Fatal(err)
		}
		if !c.Paper {
			t.Fatal("TRADE_KERNEL_LIVE=0 should force paper")
		}
		if c.Live() {
			t.Fatal("Live() should be false when forced paper")
		}
	})
}

func TestCredentialsFromConfigFile(t *testing.T) {
	// Keys defined in the YAML file load successfully with no env vars set.
	tmp := t.TempDir() + "/tk.yaml"
	if err := os.WriteFile(tmp, []byte(`
api_key_id: FILE_KEY
api_secret_key: FILE_SECRET
paper: true
`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Ensure the env vars we test against are unset (don't pollute the
	// real developer environment, and don't inherit it either).
	t.Setenv("APCA_API_KEY_ID", "")
	t.Setenv("APCA_API_SECRET_KEY", "")

	c, err := Load(tmp)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.APIKeyID != "FILE_KEY" || c.APISecretKey != "FILE_SECRET" {
		t.Fatalf("credentials = %q/%q, want FILE_KEY/FILE_SECRET", c.APIKeyID, c.APISecretKey)
	}
}

func TestCredentialsEnvOverridesFile(t *testing.T) {
	// When both file and env are set, env wins.
	tmp := t.TempDir() + "/tk.yaml"
	if err := os.WriteFile(tmp, []byte(`
api_key_id: FILE_KEY
api_secret_key: FILE_SECRET
paper: true
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("APCA_API_KEY_ID", "ENV_KEY")
	t.Setenv("APCA_API_SECRET_KEY", "ENV_SECRET")

	c, err := Load(tmp)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.APIKeyID != "ENV_KEY" || c.APISecretKey != "ENV_SECRET" {
		t.Fatalf("credentials = %q/%q, want ENV_KEY/ENV_SECRET (env must override file)", c.APIKeyID, c.APISecretKey)
	}
}

func TestCredentialsMissingBothSourcesErrors(t *testing.T) {
	// Neither file nor env provides keys ⇒ Validate errors. The message
	// should mention both accepted sources so the user knows the options.
	tmp := t.TempDir() + "/tk.yaml"
	if err := os.WriteFile(tmp, []byte("paper: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("APCA_API_KEY_ID", "")
	t.Setenv("APCA_API_SECRET_KEY", "")

	_, err := Load(tmp)
	if err == nil {
		t.Fatal("expected error when no credentials provided")
	}
	for _, want := range []string{"api_key_id", "APCA_API_KEY_ID"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q should mention %q", err.Error(), want)
		}
	}
}

func TestCredentialsEnvOnly(t *testing.T) {
	// Backward-compat: env-only (no keys in file) still works.
	tmp := t.TempDir() + "/tk.yaml"
	if err := os.WriteFile(tmp, []byte("paper: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("APCA_API_KEY_ID", "ENV_KEY")
	t.Setenv("APCA_API_SECRET_KEY", "ENV_SECRET")

	c, err := Load(tmp)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.APIKeyID != "ENV_KEY" || c.APISecretKey != "ENV_SECRET" {
		t.Fatalf("credentials = %q/%q, want ENV_KEY/ENV_SECRET", c.APIKeyID, c.APISecretKey)
	}
}
