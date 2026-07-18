# trade-kernel — Design Document

A low-latency terminal application for high-speed manual trading of US
equities on Alpaca, with full 24/5 session support. Single Go process
on a GCP VM near Alpaca's servers; the user attaches via SSH + tmux.

- **Market**: US equities, single active symbol with keyboard hot-switch.
- **Sessions**: overnight (20:00–04:00 ET), pre-market (04:00–09:30),
  regular (09:30–16:00), after-hours (16:00–20:00) — all tradable with
  session-appropriate order forms.
- **Execution**: Alpaca REST v1, behind an `Executor` interface so a FIX
  engine (quickfixgo) can slot in later without touching the UI or risk
  layers.
- **Environment**: paper by default; live requires `paper: false` plus
  `live_trading_acknowledged: true` and shows a prominent startup banner.

## Architecture

Single process, goroutines connected by channels, mutex-guarded state at
package boundaries. The render loop pulls snapshots on a 100 ms ticker
and never touches the ingest hot path.

```
SIP WS (trades/quotes) ──> bars.Aggregator ──(pull: Snapshot/LatestQuote)──┐
                                    ▲                                      │
Alpaca trading WS ──> state.Store ──┤(pull: positions/orders/account)      ├──> ui (bubbletea TUI)
REST reconcile (5 s) ──> state.Store┘                                      │
session.Engine ──(Events: transitions)──> VWAP reset, status bar           │
                                                                           │
Keyboard ──> ui.Model ──> risk.Checker ──> execution.Executor ──> Alpaca REST
                  │              ▲                  ▲
                  └── confirm / pending state       └── execution.Builder
                                                       (session-aware rules)
risk.LossMonitor <── account equity (5 s refresh) ──> kill-switch:
                                                      cancel all + flatten + lock
```

### Packages

| Package | Responsibility |
|---|---|
| `cmd/trade-kernel` | Config load, startup banner, wire-up, signal handling, symbol switching, bar backfill, kill-switch action. |
| `internal/config` | YAML config + env overrides (`APCA_API_KEY_ID`, `APCA_API_SECRET_KEY`). Validation: live mode requires explicit acknowledgement; fills defaults. |
| `internal/session` | Authoritative session classification in `America/New_York` (tz database, DST-safe). `Engine` emits transition events, accepts `/v2/clock` overrides for holidays/early closes (only ever *narrows* to Closed, never widens). |
| `internal/alpaca` | REST client (account, positions, orders, clock, assets, bars w/ pagination); SIP market-data WS client (auth, subscribe, hot-switch, exponential-backoff reconnect, resync callback); trading WS client (`trade_updates`). |
| `internal/bars` | Aggregates trades into 1s/5s/15s/1m/5m/15m/1h/1d bars in preallocated ring buffers (2048/TF). Daily bars anchor at 20:00 ET (the overnight open) so a "day" is one 24/5 trading day. Handles late/out-of-order trades (H/L/V correction in-place). Maintains per-TF SMA/EMA and session VWAP; caches latest NBBO + last trade for the order builder. |
| `internal/indicators` | Pure incremental O(1) SMA, EMA, resettable VWAP, each with non-mutating `Peek` for live forming-bar values. |
| `internal/state` | Mutex-guarded cache of account/positions/open orders. REST snapshot at startup + every 5 s; trading-WS events applied incrementally; full refresh on WS reconnect. |
| `internal/execution` | `Executor` interface (`Buy/Sell/LimitBuy/LimitSell/Flatten/CancelAll`). `Builder` applies session rules. `RESTExecutor` submits with generated client order IDs. `EligibilityCache` caches overnight-tradability per symbol (1 h TTL). |
| `internal/risk` | `Checker`: kill-switch lock, max order qty, projected max position qty (reducing exposure always allowed), duplicate-order debounce. `LossMonitor`: equity vs. first reading of each ET day; fires once per day. |
| `internal/cmdline` | `:` command parser → typed `Command` structs. |
| `internal/ui` | bubbletea model, braille candlestick renderer, volume pane, side panel, status bar, latency tracker, confirmation state machine. |

## Key design decisions

### 1. Pull-based rendering

The TUI does not consume per-trade events. Ingest goroutines write into
`bars.Aggregator` and `state.Store` (both mutex-guarded); the render
loop snapshots every 100 ms. This caps render work, keeps GC pressure
off the ingest path, and makes the UI trivially correct under bursts —
a trade burst just changes the next snapshot.

### 2. Session engine is the single source of truth for order form

Wrong session ⇒ wrong order form ⇒ rejected or unexpectedly queued
orders. Classification is pure wall-clock logic in ET (`session.At`),
unit-tested across all boundaries (04:00/09:30/16:00/20:00, Friday
20:00 close, Sunday 20:00 open, both DST transitions). The Alpaca
`/v2/clock` reading is advisory: it can force Closed (holiday) but can
never override a locally-derived Closed into a trading session.

### 3. Session-aware order builder

| Session | Hotkey order (B/S/A/D/F) | Explicit limit (`:buy 100 lmt 152.30`) |
|---|---|---|
| Regular | market, TIF=day (IOC configurable) | limit, TIF=day/ioc |
| Pre-market / after-hours | limit at far side of NBBO ± slippage, `extended_hours=true`, TIF=day | limit as given, `extended_hours=true`, TIF=day |
| Overnight | same conversion + assets-endpoint eligibility check (ineligible ⇒ reject with warning) | same |
| Closed | reject | reject |

Slippage defaults to 25 bps, rounded aggressively (buys ceil to the
cent, sells floor). If the NBBO is stale (>3 s), the builder prices off
the last trade and returns a warning that the UI surfaces before
submission. The confirmation prompt always shows the computed limit
price in extended sessions.

### 4. Safety rails (defense in depth)

1. **Pre-trade** (`risk.Checker`): max order size, projected position
   cap, 300 ms duplicate-order debounce, kill-switch lock.
2. **Daily-loss kill-switch** (`risk.LossMonitor`): equity polled every
   5 s against the first reading of the ET day; on breach → lock the
   checker, cancel all, flatten every position using the
   session-appropriate order form. Fires at most once per ET day;
   `:unlock` re-enables manually.
3. **Panic key (X)**: cancel all + flatten, bypassing the checker and
   confirmation — the emergency exit must always work.
4. **Idempotency**: every order carries a generated client order ID;
   state reconciliation after reconnect prevents duplicates.

### 5. Bar aggregation with backfill

Live trades aggregate into all 8 timeframes simultaneously (fixed-cost
per trade). On symbol switch and on every WS reconnect, history is
backfilled from the REST bars endpoint (SIP feed includes
extended-hours trades, so overnight bars exist; weekend/holiday gaps
simply don't appear — collapsed chart). Sub-minute timeframes build
from the live stream only. A reconnect triggers a backfill so
sequence gaps are healed from REST rather than interpolated.

### 6. Indicators

SMA/EMA update per bar close per timeframe from ring-buffered values
(O(1), no history scan). Session VWAP accumulates per trade and resets
on session transitions; anchor is configurable — `session` (every
transition, default) or `day` (only at the 20:00 ET overnight open).
Forming-bar values use non-mutating `Peek` so the live edge renders
without corrupting state.

### 7. Rendering

Candlesticks in braille (2×4 dots per cell → 2× horizontal, 4× vertical
resolution): wick in the left dot-column, body across both; green/red
per cell. SMA/EMA/VWAP as colored dot overlays drawn under candles.
Volume pane with eighth-block characters. Session background shading
per column (overnight = dark blue, pre/after = dark gray, toggleable
via config/`:shading`). Price range auto-scales across bars + enabled
overlays with 2% padding.

### 8. Latency discipline

- Ingest path: one mutex, no allocations per trade beyond the decoded
  WS message; ring buffers preallocated at startup.
- Order path: hotkey → risk check (in-memory) → one REST call. No
  logging on the hot path; logs go to `trade-kernel.log`.
- Keypress→ack latency recorded per order; p50/p99 shown in the status
  bar.
- `GOGC`/ballast tuning deliberately deferred: measure with
  `GODEBUG=gctrace` on the target VM first.

## Keyboard spec (defaults, all rebindable)

| Key | Action |
|---|---|
| `B` / `S` | Buy / sell preset size (market regular, aggressive limit extended) |
| `A` / `D` | Add to / reduce position (direction-aware from position sign) |
| `F` | Flatten (session-appropriate form) |
| `C` | Cancel all open orders |
| `X` | Panic: cancel all + flatten (bypasses checks/confirmation) |
| `1`–`9` | Select size preset |
| `:` | Command line: `buy 250 lmt 152.30`, `sell 100 mkt`, `sym NVDA`, `tf 5m`, `preset 2`, `flatten`, `cancel`, `unlock`, `confirm on|off`, `shading on|off`, `quit`, `help` |
| `Tab` | Cycle resolution (1m/5m/15m/1h/1d/1s/5s/15s) |
| `i` | Cycle indicator overlay combos |
| `q` / `Ctrl+C` | Quit (confirms with open position when confirmations on) / force quit |

## Configuration

`trade-kernel.yaml` (gitignored; see `config.example.yaml`) plus env
vars for credentials. Notables: `size_presets`, `limits.{max_order_qty,
max_position_qty, daily_loss_limit, debounce_ms}`,
`extended_hours.{slippage_bps, quote_stale_ms}`,
`indicators.{sma_period, ema_period, vwap_anchor}`, `confirm_orders`,
`chart.session_shading`, `keys` overrides. `TRADE_KERNEL_LIVE=1` also
forces live mode (still requires acknowledgement).

## Failure handling

| Failure | Behavior |
|---|---|
| Market-data WS drop | Exponential backoff (250 ms→10 s), re-auth, re-subscribe, REST backfill to heal gaps. |
| Trading WS drop | Same reconnect; full state reconcile from REST on re-auth. |
| App restart mid-position | Startup reconcile (account + positions + open orders) before TUI starts. |
| REST 4xx | Alpaca error envelope surfaced to the status line. |
| Stale feed | Last-trade age shown in status bar; stale NBBO triggers last-trade pricing + warning in order builder. |
| Clock skew | Session engine re-syncs `/v2/clock` every 60 s; deployment requires chrony/timesyncd. |

## Testing strategy

- `internal/session`: boundary table tests (every session edge, weekly
  open/close, DST both directions), clock-override semantics.
- `internal/bars`: canned tick sequences → OHLCV correctness, bar roll,
  late-trade correction, 20:00 daily anchor, backfill + live extension,
  snapshot limits.
- `internal/indicators`: SMA/EMA/VWAP vs. reference math; `Peek` purity.
- `internal/execution`: order-builder matrix (regular market/limit/IOC,
  extended conversion price math both sides, stale-quote fallback,
  overnight eligibility accept/reject, closed rejection).
- `internal/risk`: size caps, reduce-always-allowed, debounce window,
  lock/unlock, loss-monitor trip + daily reset.
- `internal/cmdline`: parser table tests incl. error forms.
- `internal/ui`: hotkey→executor flows with a fake executor
  (buy/sell/add/reduce/flatten/panic), confirmation state machine,
  cmdline execution, risk blocking, render smoke tests (wide + narrow).

`go build ./... && go vet ./... && go test -race ./...` all clean.

## Deployment

GCP e2-small in the region with lowest measured RTT to
`api.alpaca.markets` (validate, don't assume — script in
`deploy/SETUP.md`). systemd unit runs the binary inside a detached tmux
session with `Restart=on-failure`; secrets in a mode-600
`EnvironmentFile`. Attach: `ssh -t host 'tmux attach -t trade-kernel'`
(mosh recommended on flaky links). chrony for clock sync.

## Known limits / future work

- REST order latency (~10–50 ms) accepted for v1; the upgrade path is a
  FIX executor implementing the same `Executor` interface (quickfixgo),
  gated on Alpaca FIX eligibility.
- Overnight liquidity is thin; aggressive-limit conversion can miss
  fills — unfilled exit orders must be watched in the orders panel.
- Out of scope v1: options/crypto, bracket/OCO orders, multi-symbol
  panes, alerts, backtesting, fractional qty, custom client-server
  protocol.
