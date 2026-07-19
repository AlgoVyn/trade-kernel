// Package cmdline parses ':' commands.
package cmdline

import (
	"fmt"
	"strconv"
	"strings"
)

// Kind identifies the command type.
type Kind int

const (
	KindOrder  Kind = iota // buy/sell
	KindSymbol             // sym AAPL
	KindTF                 // tf 5m
	KindPreset             // preset 2
	KindFlatten
	KindCancel
	KindConfirm // confirm on|off
	KindShading // shading on|off
	KindLock    // lock [reason]
	KindUnlock
	KindPanic // panic (active symbol)
	KindQuit
	KindHelp
	KindFocus // focus N|off — crop chart to the last N bars
)

// Command is a parsed ':' command.
type Command struct {
	Kind   Kind
	Side   string  // KindOrder: "buy" | "sell"
	Qty    int     // KindOrder
	Limit  float64 // KindOrder: >0 = limit, 0 = market
	Symbol string  // KindSymbol
	TF     string  // KindTF
	Preset int     // KindPreset: 1-based
	On     bool    // KindConfirm / KindShading
	Reason string  // KindLock
	Focus  int     // KindFocus: bars to crop from the left (0 = off)
}

var verbs = map[string]Kind{
	"flatten": KindFlatten, "flat": KindFlatten, "f": KindFlatten,
	"cancel": KindCancel, "c": KindCancel,
	"unlock": KindUnlock,
	"quit":   KindQuit, "q": KindQuit,
	"help": KindHelp, "h": KindHelp,
}

// Parse parses input (without the leading ':').
func Parse(input string) (Command, error) {
	fields := strings.Fields(input)
	if len(fields) == 0 {
		return Command{}, fmt.Errorf("empty command")
	}
	head := strings.ToLower(fields[0])
	args := fields[1:]

	switch head {
	case "buy", "b", "sell", "s":
		return parseOrder(head, args)
	case "sym", "symbol":
		if len(args) != 1 {
			return Command{}, fmt.Errorf("usage: sym TICKER")
		}
		return Command{Kind: KindSymbol, Symbol: strings.ToUpper(args[0])}, nil
	case "tf":
		if len(args) != 1 {
			return Command{}, fmt.Errorf("usage: tf 1s|5s|15s|1m|5m|15m|1h|1d|2m|30s|…")
		}
		return Command{Kind: KindTF, TF: strings.ToLower(args[0])}, nil
	case "preset", "p":
		if len(args) != 1 {
			return Command{}, fmt.Errorf("usage: preset N")
		}
		n, err := strconv.Atoi(args[0])
		if err != nil || n < 1 {
			return Command{}, fmt.Errorf("preset must be a positive number")
		}
		return Command{Kind: KindPreset, Preset: n}, nil
	case "focus":
		if len(args) != 1 {
			return Command{}, fmt.Errorf("usage: focus N|off")
		}
		if strings.EqualFold(args[0], "off") {
			return Command{Kind: KindFocus, Focus: 0}, nil
		}
		n, err := strconv.Atoi(args[0])
		if err != nil || n < 1 {
			return Command{}, fmt.Errorf("focus must be a positive number or off")
		}
		return Command{Kind: KindFocus, Focus: n}, nil
	case "confirm", "shading":
		if len(args) != 1 || (args[0] != "on" && args[0] != "off") {
			return Command{}, fmt.Errorf("usage: %s on|off", head)
		}
		k := KindConfirm
		if head == "shading" {
			k = KindShading
		}
		return Command{Kind: k, On: args[0] == "on"}, nil
	case "lock":
		reason := "manual"
		if len(args) > 0 {
			reason = strings.Join(args, " ")
		}
		return Command{Kind: KindLock, Reason: reason}, nil
	case "panic":
		if len(args) != 0 {
			return Command{}, fmt.Errorf("usage: panic")
		}
		return Command{Kind: KindPanic}, nil
	default:
		if k, ok := verbs[head]; ok {
			return Command{Kind: k}, nil
		}
		return Command{}, fmt.Errorf("unknown command %q (try :help)", head)
	}
}

func parseOrder(verb string, args []string) (Command, error) {
	side := "buy"
	if verb == "sell" || verb == "s" {
		side = "sell"
	}
	if len(args) < 1 || len(args) > 3 {
		return Command{}, fmt.Errorf("usage: %s QTY [lmt PRICE|mkt]", verb)
	}
	qty, err := strconv.Atoi(args[0])
	if err != nil || qty <= 0 {
		return Command{}, fmt.Errorf("qty must be a positive integer")
	}
	c := Command{Kind: KindOrder, Side: side, Qty: qty}
	if len(args) == 1 {
		return c, nil // market (session-aware conversion)
	}
	switch strings.ToLower(args[1]) {
	case "mkt":
		if len(args) != 2 {
			return Command{}, fmt.Errorf("usage: %s QTY mkt", verb)
		}
		return c, nil
	case "lmt", "limit":
		if len(args) != 3 {
			return Command{}, fmt.Errorf("usage: %s QTY lmt PRICE", verb)
		}
		p, err := strconv.ParseFloat(args[2], 64)
		if err != nil || p <= 0 {
			return Command{}, fmt.Errorf("invalid limit price %q", args[2])
		}
		c.Limit = p
		return c, nil
	default:
		return Command{}, fmt.Errorf("expected lmt PRICE or mkt, got %q", args[1])
	}
}
