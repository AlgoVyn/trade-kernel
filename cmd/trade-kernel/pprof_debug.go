package main

import (
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"strings"
)

// Optional debug aid: TK_PPROF=localhost:6060 (or 127.0.0.1 / ::1) exposes
// net/http/pprof. Non-loopback binds are refused so a networked host cannot
// accidentally publish profiles.
func init() {
	addr := strings.TrimSpace(os.Getenv("TK_PPROF"))
	if addr == "" {
		return
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// Allow bare ":6060" only if we still require explicit loopback — reject.
		log.Printf("pprof: TK_PPROF must be host:port (loopback only), got %q: %v", addr, err)
		return
	}
	if !isLoopbackHost(host) {
		log.Printf("pprof: refusing non-loopback address %q (use localhost:6060)", addr)
		return
	}
	if port == "" {
		log.Printf("pprof: missing port in %q", addr)
		return
	}
	go func() {
		if err := http.ListenAndServe(addr, nil); err != nil {
			log.Printf("pprof: %v", err)
		}
	}()
}

func isLoopbackHost(host string) bool {
	h := strings.TrimSpace(host)
	switch strings.ToLower(h) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
