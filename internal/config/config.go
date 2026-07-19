// Package config loads trade-kernel configuration from a YAML file and
// environment variables. Environment variables always win over file values.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration for trade-kernel.
type Config struct {
	// Paper selects the paper trading endpoints. Live trading requires
	// paper: false AND live_trading_acknowledged: true.
	Paper                   bool   `yaml:"paper"`
	LiveTradingAcknowledged bool   `yaml:"live_trading_acknowledged"`
	APIKeyID                string `yaml:"api_key_id"`
	APISecretKey            string `yaml:"api_secret_key"`

	DefaultSymbol string `yaml:"default_symbol"`

	// SizePresets are selectable via keys 1-4. First entry is the default.
	SizePresets []int `yaml:"size_presets"`

	ConfirmOrders bool `yaml:"confirm_orders"`

	Limits Limits `yaml:"limits"`

	ExtendedHours ExtendedHours `yaml:"extended_hours"`

	Indicators Indicators `yaml:"indicators"`

	Chart Chart `yaml:"chart"`

	Keys map[string]string `yaml:"keys"`
}

// Limits are the pre-trade safety rails.
type Limits struct {
	MaxOrderQty    int `yaml:"max_order_qty"`
	MaxPositionQty int `yaml:"max_position_qty"`
	DebounceMS     int `yaml:"debounce_ms"` // duplicate-order debounce window
}

// ExtendedHours configures order conversion outside regular hours.
type ExtendedHours struct {
	// SlippageBps is added to (buys) / subtracted from (sells) the far
	// side of the NBBO when building aggressive limits.
	SlippageBps float64 `yaml:"slippage_bps"`
	// QuoteStaleMS is the age after which an NBBO quote is considered
	// stale and the builder falls back to the last trade price.
	QuoteStaleMS int `yaml:"quote_stale_ms"`
}

// Indicators configures overlay periods and VWAP anchoring.
// Two independent EMAs are drawn (e.g. fast 9 + slow 20).
type Indicators struct {
	EMAPeriod  int `yaml:"ema_period"`  // fast EMA (default 9)
	EMA2Period int `yaml:"ema2_period"` // slow EMA (default 20)
	// SMAPeriod is a deprecated alias for ema2_period (accepted when
	// ema2_period is unset so older YAML keeps controlling the slow MA).
	SMAPeriod int `yaml:"sma_period"`
	// VWAPAnchor: "session" (default) or "day".
	VWAPAnchor string `yaml:"vwap_anchor"`
}

// Chart configures rendering.
type Chart struct {
	Timeframe      string `yaml:"timeframe"` // initial resolution
	BarsVisible    int    `yaml:"bars_visible"`
	SessionShading bool   `yaml:"session_shading"`
}

func (c Config) QuoteStaleAfter() time.Duration {
	ms := c.ExtendedHours.QuoteStaleMS
	if ms <= 0 {
		ms = 3000
	}
	return time.Duration(ms) * time.Millisecond
}

func (c Config) Debounce() time.Duration {
	ms := c.Limits.DebounceMS
	if ms <= 0 {
		ms = 300
	}
	return time.Duration(ms) * time.Millisecond
}

// Live returns true when configured for live trading (paper off and
// explicitly acknowledged).
func (c Config) Live() bool { return !c.Paper && c.LiveTradingAcknowledged }

// Validate enforces invariants and fills defaults.
func (c *Config) Validate() error {
	if c.APIKeyID == "" || c.APISecretKey == "" {
		return fmt.Errorf("API credentials missing: set api_key_id/api_secret_key in the config file or APCA_API_KEY_ID/APCA_API_SECRET_KEY in the environment")
	}
	if !c.Paper && !c.LiveTradingAcknowledged {
		return fmt.Errorf("paper: false requires live_trading_acknowledged: true; refusing to start")
	}
	if len(c.SizePresets) == 0 {
		c.SizePresets = []int{100, 250, 500, 1000}
	}
	for i, p := range c.SizePresets {
		if p <= 0 {
			return fmt.Errorf("size_presets[%d] must be positive", i)
		}
	}
	if len(c.SizePresets) > 9 {
		return fmt.Errorf("at most 9 size presets supported")
	}
	if c.DefaultSymbol == "" {
		c.DefaultSymbol = "AAPL"
	}
	if c.Indicators.EMAPeriod <= 0 {
		c.Indicators.EMAPeriod = 9
	}
	if c.Indicators.EMA2Period <= 0 {
		// Deprecated sma_period alias → slow EMA when ema2_period omitted.
		if c.Indicators.SMAPeriod > 0 {
			c.Indicators.EMA2Period = c.Indicators.SMAPeriod
		} else {
			c.Indicators.EMA2Period = 20
		}
	}
	if c.Indicators.VWAPAnchor == "" {
		c.Indicators.VWAPAnchor = "session"
	}
	if c.Indicators.VWAPAnchor != "session" && c.Indicators.VWAPAnchor != "day" {
		return fmt.Errorf("vwap_anchor must be \"session\" or \"day\"")
	}
	if c.Chart.Timeframe == "" {
		c.Chart.Timeframe = "1m"
	}
	if c.Chart.BarsVisible <= 0 {
		c.Chart.BarsVisible = 120
	}
	// Safety rails: negative values would silently disable checks in
	// risk.Checker (which treats <=0 as "disabled"). Reject explicitly so a
	// config typo can't quietly turn off a guardrail.
	if c.Limits.MaxOrderQty < 0 {
		return fmt.Errorf("limits.max_order_qty must be >= 0 (0 disables)")
	}
	if c.Limits.MaxPositionQty < 0 {
		return fmt.Errorf("limits.max_position_qty must be >= 0 (0 disables)")
	}
	return nil
}

// Load reads the YAML file at path (missing file is allowed) and applies
// environment overrides.
func Load(path string) (Config, error) {
	var c Config
	c.Paper = true // safe default
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				return c, fmt.Errorf("read config: %w", err)
			}
		} else if err := yaml.Unmarshal(b, &c); err != nil {
			return c, fmt.Errorf("parse config %s: %w", path, err)
		}
	}
	if v := os.Getenv("APCA_API_KEY_ID"); v != "" {
		c.APIKeyID = v
	}
	if v := os.Getenv("APCA_API_SECRET_KEY"); v != "" {
		c.APISecretKey = v
	}
	// TRADE_KERNEL_LIVE overrides paper mode from the environment:
	// "1" forces live (still requires live_trading_acknowledged to start),
	// "0" forces paper. Unset leaves the file value alone.
	switch os.Getenv("TRADE_KERNEL_LIVE") {
	case "1":
		c.Paper = false
	case "0":
		c.Paper = true
	}
	if err := c.Validate(); err != nil {
		return c, err
	}
	return c, nil
}
