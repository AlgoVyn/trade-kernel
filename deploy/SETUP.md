# trade-kernel deployment notes (GCP + tmux)

The app runs entirely on a GCP VM near Alpaca's servers; you attach
remotely over SSH + tmux. There is no client/server protocol of our own.

## 1. Pick the region by measurement (do not assume)

Spin up cheap spot instances in candidate regions (start with
`us-east4` — Ashburn, commonly cited for Alpaca) and measure RTT to
both endpoints:

```bash
for i in 1 2 3 4 5; do
  curl -o /dev/null -s -w "api   %{time_total}s\n" https://api.alpaca.markets/v2/clock
  curl -o /dev/null -s -w "data  %{time_total}s\n" https://data.alpaca.markets/v2/stocks/AAPL/bars/latest
done
```

Pick the region with the lowest, most stable times, then provision the
permanent VM there.

## 2. VM

- `e2-small` suffices for v1. Debian/Ubuntu minimal image.
- Install: `sudo apt-get install -y tmux chrony` (mosh optional for
  flaky networks: `sudo apt-get install -y mosh`).
- Time sync matters (bar timestamps, logs): verify
  `timedatectl` shows `System clock synchronized: yes` (chrony or
  systemd-timesyncd).

## 3. Install

```bash
sudo useradd -r -m -d /opt/trade-kernel trade
sudo -u trade mkdir -p /opt/trade-kernel
# copy the binary (build with: GOOS=linux GOARCH=amd64 go build ./cmd/trade-kernel)
sudo install -m 755 trade-kernel /opt/trade-kernel/trade-kernel

sudo mkdir -p /etc/trade-kernel
sudo cp trade-kernel.yaml /etc/trade-kernel/            # your config
sudo tee /etc/trade-kernel/env >/dev/null <<EOF
APCA_API_KEY_ID=...
APCA_API_SECRET_KEY=...
EOF
sudo chmod 600 /etc/trade-kernel/env
sudo cp deploy/trade-kernel.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now trade-kernel
```

Secrets live only in `/etc/trade-kernel/env` (mode 600) or GCP Secret
Manager — never in git.

## 4. Attach

```bash
ssh -t <host> 'tmux attach -t trade-kernel'
```

`Ctrl-b d` detaches and leaves the app running; systemd restarts it on
crash. Consider `mosh <host> -- tmux attach -t trade-kernel` on
unstable links.

## 5. Paper → live

Run paper until the validation checklist passes. Then edit
`/etc/trade-kernel/trade-kernel.yaml`:

```yaml
paper: false
live_trading_acknowledged: true
```

The startup banner shows the mode; live mode prints an additional
warning and pauses 2s before starting.
