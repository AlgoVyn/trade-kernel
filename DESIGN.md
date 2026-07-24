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
package boundaries. The render loop pulls snapshots on a configurable
tick (`chart.tick_ms`, default 50 ms, adaptive by TF) and never touches
the ingest hot path.

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
```

### Packages

| Package | Responsibility |
|---|---|
| `cmd/trade-kernel` | Config load, startup banner, wire-up, signal handling, symbol switching, bar backfill. |
| `internal/config` | YAML config (incl. `api_key_id`/`api_secret_key`) + env overrides (`APCA_API_KEY_ID`, `APCA_API_SECRET_KEY`; env wins). Validation: live mode requires explicit acknowledgement; fills defaults. |
| `internal/session` | Authoritative session classification in `America/New_York` (tz database, DST-safe). `Engine` emits transition events, accepts `/v2/clock` overrides for holidays/early closes (only ever *narrows* to Closed, never widens). |
| `internal/alpaca` | REST client (account, positions, orders, clock, assets, portfolio history, bars w/ pagination); SIP market-data WS client (auth, subscribe, hot-switch, exponential-backoff reconnect, resync callback); trading WS client (`trade_updates`). |
| `internal/bars` | Aggregates trades into 1s/5s/15s/1m/5m/15m/1h/1d bars in preallocated ring buffers (2048/TF). Daily bars anchor at 20:00 ET (the overnight open) so a "day" is one 24/5 trading day. Handles late/out-of-order trades (H/L/V correction in-place). Maintains per-TF dual EMA and session VWAP; caches latest NBBO + last trade for the order builder. |
| `internal/indicators` | Pure incremental O(1) EMA, resettable VWAP (SMA retained as a utility), each with non-mutating `Peek` for live forming-bar values. |
| `internal/state` | Mutex-guarded cache of account/positions/open orders. REST snapshot at startup + every 5 s; trading-WS events applied incrementally; full refresh on WS reconnect. Account **realized day/week PnL** from closed **order fills** (avg-cost inventory over FILL activities with open-position cost-basis seeds from the last REST reconcile for pre-lookback lots; retained REST avg after positions go flat so pure-exit books of previously seeded names stay continuous; fallback closed orders) — not equity MTM. Symbols with opposite-side seed/fill disagreement or ghost inventory without retained avg (full exit of a lot opened before the ~180d lookback with unknown cost basis) are excluded — partial samples log a soft warning and the TUI marks `rday*`/`rwk*`. Same-side REST-ahead-of-FILL lag trusts the fill book. If nothing trustworthy remains (or a fetch fails), the prior sample is kept; `PnL()` hides figures older than the stale threshold. FILL buffer is cached and delta-fetched on the 60 s refresh (empty successful caches re-poll a short tail, with a periodic full lookback heal). `rday` / `rwk` bucket by `DayStart` / `WeekStart` (last real overnight open Sun–Thu 20:00 ET, weekend pins to Thu 20:00 so Friday rday stays visible / Sunday 20:00 week). POS still shows open unrealized. |
| `internal/execution` | `Executor` interface (`Buy/Sell/LimitBuy/LimitSell/Flatten/CancelAll/CancelSymbol`). `Builder` applies session rules. `RESTExecutor` submits with generated client order IDs. `EligibilityCache` caches overnight-tradability per symbol (1 h TTL). |
| `internal/risk` | `Checker`: optional lock, max order qty, projected max position qty including same-side resting orders (strict same-sign reduce while over cap allowed; equal-magnitude or oversize reverse while over cap is rejected), duplicate-order debounce (Check/Record split). Production flatten/panic do **not** invoke the checker (full bypass); `CheckOpts` Skip* flags remain for tests / future gated exits. |
| `internal/cmdline` | `:` command parser → typed `Command` structs. |
| `internal/ui` | bubbletea model, solid-block candlestick renderer with braille indicator overlays, volume pane, bottom info bar, status bar, latency tracker, confirmation state machine. Cancel (C) and panic (X) are symbol-scoped via `CancelSymbol` (+ flatten for panic). |

## Key design decisions

### 1. Pull-based rendering

The TUI does not consume per-trade events. Ingest goroutines write into
`bars.Aggregator` and `state.Store` (both mutex-guarded); the render
loop snapshots on a short adaptive ticker. This caps render work, keeps
GC pressure off the ingest path, and makes the UI trivially correct under
bursts — a trade burst just changes the next snapshot.

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
| Regular | market, TIF=`orders.regular_tif` (`day` default, or `ioc`) | limit, same TIF |
| Pre-market / after-hours | limit at far side of NBBO ± slippage, `extended_hours=true`, TIF=day | limit as given, `extended_hours=true`, TIF=day |
| Overnight | same conversion + assets-endpoint eligibility check (ineligible ⇒ reject with warning) | same |
| Closed | reject | reject |

Slippage defaults to 25 bps, rounded aggressively (buys ceil to the
cent, sells floor). If the NBBO is stale (>3 s), the builder prices off
the last trade and returns a warning that the UI surfaces before
submission. **Flatten/panic** additionally allow a last trade up to
~5 minutes old on quiet extended/overnight tape (`AllowStaleLastTrade`)
so exits do not hard-fail when the book is empty. The confirmation
prompt always shows the computed limit price in extended sessions
(using the same exit pricing budget for flatten).

### 4. Safety rails (defense in depth)

1. **Pre-trade** (`risk.Checker`): max order size, projected position
   cap (**open position + same-side resting open-order qty + new order**),
   300 ms duplicate-order debounce, manual kill-switch (`:lock [reason]` /
   `:unlock`). Opposite-side resting orders do not create credit against
   the cap. There is no automatic daily-loss lock; operators engage
   the switch deliberately. **Flatten and panic do not call the checker**
   (full bypass so exits work under lock, over size caps, and with stacked
   working orders). New risk-increasing orders stay blocked while locked.
2. **Panic key (X / `:panic`)**: cancel open orders for the *active*
   symbol + flatten that symbol only, bypassing the checker and
   confirmation. Scope is always the selected symbol (switch with
   `:sym` and panic each name separately). When other symbols remain
   open, the status line reminds the operator (`:sym` then `X`).
   Flatten (F / `:flatten` / panic) uses the session-appropriate order
   form for both long and short exits — market in Regular, aggressive
   limit with `extended_hours=true` in extended sessions. It never falls
   back to `DELETE /v2/positions` outside regular hours (that endpoint
   liquidates at market, which extended sessions do not allow); that
   endpoint is reserved for the Closed session and fractional
   quantities, where the order path cannot work. Submit **re-reads**
   position size via REST `GET /v2/positions/{symbol}` first (local
   store only if REST fails) so a confirm delay, unreconciled book, or
   stale-high local qty cannot leave residual size or oversize the
   exit. Flatten/panic regular-hours exits always use **day** TIF
   (`orders.regular_tif` / IOC applies to hotkey `:` buy-sell only).
   Session class is **pinned at intent/confirm** so a 09:30/16:00
   boundary during y/n cannot flip limit↔market.
3. **Idempotency**: every order carries a generated client order ID;
   state reconciliation after reconnect prevents duplicates.

### 5. Bar aggregation with backfill

Live trades aggregate into all 8 timeframes simultaneously (fixed-cost
per trade). On symbol switch and on every WS reconnect, history is
backfilled from the REST bars endpoint (SIP feed includes
extended-hours trades, so overnight bars exist; weekend/holiday gaps
simply don't appear — collapsed chart). TF bar fetches run **in
parallel** (bounded concurrency); SIP+BOATS for a given TF stay
sequential. While REST is in flight, `Aggregator.BeginBackfill` buffers
live `OnTrade` prints (still updating last price for order pricing);
after `Load`/`ReplayTrades`, `EndBackfillAfterReplay` applies the
buffer so multi-second fetches cannot leave permanent volume holes —
minute+/VWAP only prints after the REST bar-request end, sub-minute
only prints after the REST trade window (already rebuilt by
`ReplayTrades`). That dual cutoff prevents overnight BOATS (and any
path that buffers historical prints during `BeginBackfill`) from
double-counting volume already present in `Load`. The incomplete
current bucket from REST is kept as the forming bar so live trades
extend it — closing it into the ring would duplicate the minute and
mis-state volume vs SIP/TradingView. Session VWAP seeds the forming
bar's volume on Load so a mid-bar reconnect does not permanently
under-count that minute. Sub-minute timeframes (1s/5s/15s) are
backfilled by replaying recent trades from the REST trades endpoint
through the aggregator (the bars endpoint doesn't serve sub-minute
resolutions); the replay touches only the sub-minute series, never the
1m+ bars or the live session VWAP. A reconnect triggers a backfill so
sequence gaps are healed from REST rather than interpolated.

**Symbol switch** is transactional: `activeSymbol` and aggregator reset
commit only after market-data subscribe succeeds; on subscribe or
*critical* backfill failure (TF1m hard-failed with no bars) the previous
symbol is restored so the UI and SIP filter stay aligned. Soft SIP
errors on coarser TFs do not roll back a partial-but-usable switch. The
overnight poller high-water is symbol-scoped (`backfillHighWater`) so a
switch cannot inherit the previous symbol's clock.

Alpaca's market-data websocket (SIP) does not stream overnight
(20:00–04:00 ET) prints; overnight data exists only on the REST BOATS
feed. During the Overnight session a poller fetches BOATS trades every
2 s and folds new prints through `Aggregator.OnTrade` exactly like live
tape — the forming candle updates in place, new candles roll on bucket
boundaries, and volume / last price / session VWAP advance. Each tick
also refreshes the NBBO cache from the BOATS latest-quote endpoint
(**one-sided quotes accepted**, same as SIP) so extended-hours order
pricing has a live quote overnight. An **exclusive** high-water timestamp
cursor (trades at or before the mark are skipped) plus a bounded trade-ID
dedupe set prevents double application across polls and after backfill.
The backfill publishes the REST trade-window end as a symbol-scoped cursor
so polled prints never overlap trades already applied via Load/Replay.
Outside Overnight the poller idles (SIP streams the other sessions;
BOATS covers only overnight).

### 6. Indicators

Two EMAs (fast/slow, configurable periods) update per bar close per
timeframe from ring-buffered values (O(1), no history scan). Session
VWAP accumulates per trade and resets on session transitions; anchor is
configurable — `session` (every transition, default) or `day` (only at
the 20:00 ET overnight open). Forming-bar values use non-mutating `Peek`
so the live edge renders without corrupting state.

### 7. Rendering

Candlesticks in braille (2×4 dots per cell → 2× horizontal, 4× vertical
resolution): wick in the left dot-column, body across both; green/red
per cell. EMA/EMA2/VWAP as colored dot overlays drawn under candles.
Volume pane with eighth-block characters. Session background shading
per column (overnight = dark blue, pre/after = dark gray, toggleable
via config/`:shading`). Price range auto-scales across bars + enabled
overlays with 2% padding. **Focus mode** (`[` / `]` or `:focus N|off`)
crops the chart window from the left edge while keeping the right edge
pinned at live; `barCol` right-aligns so fewer bars leaves blank space
on the left and `volumeScale`/`priceRange` rebase onto the remaining
(cropped) window. Use it when a new low-volume session starts and the
prior session's peaks would otherwise squash the new session's volume
bars flat. Reset to 0 wherever `panOffset` is reset (symbol/TF switch).

### 8. Latency discipline

- Ingest path: one mutex, no allocations per trade beyond the decoded
  WS message; ring buffers preallocated at startup.
- Order path: hotkey → risk check (in-memory) → one REST call. No
  logging on the hot path; logs go to `trade-kernel.log`.
- Keypress→ack (or confirm-accept→ack) latency recorded per order from
  submit commit through broker HTTP response; p50/p99 in the status bar.
- Trading REST client: keep-alive + long idle timeout; startup
  `/v2/clock` warms TLS. Overnight eligibility is prefetched on symbol
  switch so hotkeys do not serialize `GET /assets` before `POST /orders`.
- UI pull tick: `chart.tick_ms` (default 50 ms), adaptive — short TFs
  speed up to 33 ms when base is higher (aggressive configs below 33 ms
  stay as-is); high TFs / pan / closed slow to ≥100 ms. Quiet-tape status
  price reuses the frame chart Snapshot when live; when panned, takes a
  live-edge Snapshot so history does not overwrite the quote.
- Chart/volume cell runs use **pre-baked ANSI open/close sequences** (hex
  → terminal profile converted once at init). The render path must not
  call `lipgloss.Style.Render` per run — that re-parses colors every frame
  and was measured at ~1 core at 20 Hz.
- Frame stack uses plain `"\n"` joins, not `lipgloss.JoinVertical`, so the
  render path does not re-run ANSI `stringWidth` over the full chart.
- `GOGC`/ballast tuning deliberately deferred: measure with
  `GODEBUG=gctrace` on the target VM first.
- Local laptop vs near-Alpaca VM: app optimizations remove *extra* RTTs
  and UI floor; geo still sets the order RTT floor (see `deploy/SETUP.md`).

## Keyboard spec (defaults, all rebindable)

| Key | Action |
|---|---|
| `B` / `S` | Buy / sell preset size (market regular, aggressive limit extended) |
| `A` / `D` | Add to / reduce position (direction-aware from position sign) |
| `F` | Flatten (session-appropriate form) |
| `C` | Cancel open orders for the active symbol |
| `X` | Panic: cancel active-symbol orders + flatten active symbol (bypasses checks/confirmation) |
| `1`–`9` | Select size preset |
| `:` | Command line: `buy 250 lmt 152.30`, `sell 100 mkt`, `sym NVDA`, `tf 5m`, `preset 2`, `flatten`, `cancel`, `lock [reason]`, `unlock`, `panic`, `confirm on|off`, `shading on|off`, `focus N\|off`, `quit`, `help` |
| `Tab` | Cycle resolution (1m/5m/15m/1h/1d/1s/5s/15s) |
| `Shift+Tab` | Cycle resolution backward |
| `←` / `→` | Pan chart back into history / forward toward live |
| `[` / `]` | Focus: crop chart toward the live edge / widen back (rebases volume/price scale onto the recent window when a new low-volume session starts) |
| `i` | Cycle indicator overlay combos |
| `q` / `Ctrl+C` | Quit (confirms with open position when confirmations on) / force quit |

## Configuration

`trade-kernel.yaml` (gitignored; see `config.example.yaml`) holds all
settings including `api_key_id`/`api_secret_key`. Env vars
(`APCA_API_KEY_ID`, `APCA_API_SECRET_KEY`) override the file when set.
Notables: `size_presets`, `orders.regular_tif` (`day`|`ioc`),
`limits.{max_order_qty, max_position_qty, debounce_ms}`,
`extended_hours.{slippage_bps, quote_stale_ms}`,
`indicators.{ema_period, ema2_period, vwap_anchor}` (`sma_period` is a
deprecated alias for `ema2_period`), `confirm_orders`,
`chart.session_shading`, `keys` overrides. `TRADE_KERNEL_LIVE=1` also
forces live mode (still requires acknowledgement).

## Failure handling

| Failure | Behavior |
|---|---|
| Market-data WS drop | Exponential backoff (250 ms→10 s; resets only after an authed session lasts ≥5 s), re-auth, re-subscribe, forced REST backfill on reconnect (not debounced) to heal gaps. |
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
- `internal/indicators`: EMA/VWAP vs. reference math; `Peek` purity.
- `internal/execution`: order-builder matrix (regular market/limit/IOC,
  extended conversion price math both sides, stale-quote fallback,
  overnight eligibility accept/reject, closed rejection).
- `internal/risk`: size caps (incl. working orders), same-sign reduce while
  over cap (equal-magnitude / oversize reverse rejected), debounce window,
  lock/unlock, CheckOpts Skip* flags for tests / gated exits.
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
  Quiet tape may still price exits off a last trade up to ~5 minutes
  old (with a status warning); beyond that, use the broker UI or wait
  for RTH.
- Out of scope v1: options/crypto, bracket/OCO orders, multi-symbol
  panes, alerts, backtesting, fractional qty, custom client-server
  protocol.
