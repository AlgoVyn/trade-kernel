package config

import (
	"os"
	"strings"
	"testing"
	"time"
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

func TestValidateKeyActions(t *testing.T) {
	base := Config{APIKeyID: "k", APISecretKey: "s", Paper: true}
	// Known actions + legacy aliases OK.
	c := base
	c.Keys = map[string]string{
		"buy": "B", "cancel_all": "C", "panic_all": "ctrl+x",
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid keys: %v", err)
	}
	// Unknown action rejected.
	c = base
	c.Keys = map[string]string{"nope": "Z"}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "unknown action") {
		t.Fatalf("want unknown action error, got %v", err)
	}
}

func TestValidateTimeframe(t *testing.T) {
	base := Config{APIKeyID: "k", APISecretKey: "s", Paper: true}
	// Empty → default 1m.
	c := base
	if err := c.Validate(); err != nil {
		t.Fatalf("default: %v", err)
	}
	if c.Chart.Timeframe != "1m" {
		t.Fatalf("Timeframe = %q, want 1m default", c.Chart.Timeframe)
	}
	// Built-in and custom accepted.
	for _, tf := range []string{"5m", "1s", "2m", "30s", "4h"} {
		c = base
		c.Chart.Timeframe = tf
		if err := c.Validate(); err != nil {
			t.Fatalf("timeframe %q should validate: %v", tf, err)
		}
	}
	// Garbage rejected.
	c = base
	c.Chart.Timeframe = "nope"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "timeframe") {
		t.Fatalf("want timeframe error, got %v", err)
	}
}

func TestValidateBarsVisible(t *testing.T) {
	base := Config{APIKeyID: "k", APISecretKey: "s", Paper: true}
	// Omit / zero → stay 0 (UI fills full chart width).
	c := base
	if err := c.Validate(); err != nil {
		t.Fatalf("default: %v", err)
	}
	if c.Chart.BarsVisible != 0 {
		t.Fatalf("BarsVisible = %d, want 0 (fill width)", c.Chart.BarsVisible)
	}
	// Positive cap preserved.
	c = base
	c.Chart.BarsVisible = 80
	if err := c.Validate(); err != nil {
		t.Fatalf("positive: %v", err)
	}
	if c.Chart.BarsVisible != 80 {
		t.Fatalf("BarsVisible = %d, want 80", c.Chart.BarsVisible)
	}
	// Negative rejected.
	c = base
	c.Chart.BarsVisible = -1
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "bars_visible") {
		t.Fatalf("want bars_visible error, got %v", err)
	}
}

func TestValidateTickMS(t *testing.T) {
	base := Config{APIKeyID: "k", APISecretKey: "s", Paper: true}
	// Zero → default 50.
	c := base
	if err := c.Validate(); err != nil {
		t.Fatalf("default tick: %v", err)
	}
	if c.Chart.TickMS != 50 {
		t.Fatalf("TickMS = %d, want 50 default", c.Chart.TickMS)
	}
	if c.Tick() != 50*time.Millisecond {
		t.Fatalf("Tick() = %v", c.Tick())
	}
	// Out of range rejected.
	c = base
	c.Chart.TickMS = 10
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "tick_ms") {
		t.Fatalf("want tick_ms error, got %v", err)
	}
	c = base
	c.Chart.TickMS = 501
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for tick_ms > 500")
	}
}

// TestTickClampsUnvalidated ensures Tick() is safe when Validate was skipped.
func TestTickClampsUnvalidated(t *testing.T) {
	cases := []struct {
		ms   int
		want time.Duration
	}{
		{0, 50 * time.Millisecond},
		{-5, 50 * time.Millisecond},
		{10, 16 * time.Millisecond},
		{16, 16 * time.Millisecond},
		{100, 100 * time.Millisecond},
		{500, 500 * time.Millisecond},
		{900, 500 * time.Millisecond},
	}
	for _, tc := range cases {
		c := Config{Chart: Chart{TickMS: tc.ms}}
		if g := c.Tick(); g != tc.want {
			t.Fatalf("TickMS=%d: Tick()=%v, want %v", tc.ms, g, tc.want)
		}
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
