// Package config loads trade-kernel configuration from a YAML file and
// environment variables. Environment variables always win over file values.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"trade-kernel/internal/bars"
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
	// Timeframe is the initial chart resolution: built-ins (1s, 5s, 15s,
	// 1m, 5m, 15m, 1h, 1d) or a custom duration (e.g. 2m, 3m, 30s, 4h).
	// Change at runtime with :tf <resolution>.
	Timeframe string `yaml:"timeframe"`
	// BarsVisible optionally caps how many bars the chart paints (CPU/history).
	// 0 or omit = fill the full chart width (no cap). Positive N = paint at most
	// min(width-fit, N); when N is below the width-fit count the left gutter is blank.
	// Use focus mode ([ / ] or :focus) for intentional left cropping without a hard cap.
	BarsVisible    int  `yaml:"bars_visible"`
	SessionShading bool `yaml:"session_shading"`
	// TickMS is the base UI render interval in milliseconds (default 50).
	// Short timeframes refresh slightly faster (capped at 33ms when base is
	// higher; aggressive configs below 33ms are honored). Panned history and
	// closed sessions slow to ≥100ms. Range: 16–500.
	TickMS int `yaml:"tick_ms"`
}

// Tick returns the base UI render interval.
// Matches Validate bounds (16–500): zero/negative → 50 default; out of range
// is clamped so callers outside Load/Validate never get a silent absurd value.
func (c Config) Tick() time.Duration {
	ms := c.Chart.TickMS
	if ms <= 0 {
		ms = 50
	}
	if ms < 16 {
		ms = 16
	}
	if ms > 500 {
		ms = 500
	}
	return time.Duration(ms) * time.Millisecond
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
	} else if _, ok := bars.ParseChartTF(c.Chart.Timeframe); !ok {
		return fmt.Errorf("chart.timeframe %q is invalid (use 1s|5s|15s|1m|5m|15m|1h|1d or a custom duration like 2m, 30s, 4h)", c.Chart.Timeframe)
	}
	// BarsVisible ≤0 stays 0: UI treats that as "no cap" (fill width).
	// Reject absurd positives so a typo cannot request millions of bars.
	if c.Chart.BarsVisible < 0 {
		return fmt.Errorf("chart.bars_visible must be >= 0 (0 = fill width, got %d)", c.Chart.BarsVisible)
	}
	if c.Chart.TickMS == 0 {
		c.Chart.TickMS = 50
	}
	if c.Chart.TickMS < 16 || c.Chart.TickMS > 500 {
		return fmt.Errorf("chart.tick_ms must be between 16 and 500 (got %d)", c.Chart.TickMS)
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
	for action := range c.Keys {
		if !knownKeyAction(action) {
			return fmt.Errorf("keys: unknown action %q (valid: buy, sell, add, reduce, flatten, cancel, panic, cmdline, cycle_tf, cycle_tf_back, pan_left, pan_right, cycle_indicators, quit, quit_force; legacy aliases: cancel_all, panic_all)", action)
		}
	}
	return nil
}

// knownKeyAction reports whether action is a supported keys: map value
// (including legacy cancel_all / panic_all aliases).
func knownKeyAction(action string) bool {
	switch action {
	case "buy", "sell", "add", "reduce", "flatten",
		"cancel", "cancel_all", "panic", "panic_all",
		"cmdline", "cycle_tf", "cycle_tf_back",
		"pan_left", "pan_right", "cycle_indicators",
		"quit", "quit_force":
		return true
	default:
		return false
	}
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
