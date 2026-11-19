package main

import (
	"context"
	"database/sql"
	"net"
	"os"
	"time"

	_ "modernc.org/sqlite"
)

// notifyWatchdog sends a WATCHDOG=1 ping to systemd via $NOTIFY_SOCKET.
// It is a no-op when the process is not running under systemd (no socket
// set) or the socket cannot be reached. It never logs and never fails.
func notifyWatchdog() {
	sock := os.Getenv("NOTIFY_SOCKET")
	if sock == "" {
		return
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: sock, Net: "unixgram"})
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("WATCHDOG=1"))
}

// startWatchdogHeartbeat reports liveness to systemd every 10 seconds.
// Before each ping it probes the database with a short timeout; the ping
// is only sent when the database answers. If the single SQLite connection
// is stuck (for example held by a deadlocked query), the probe times out,
// the pings stop, and systemd restarts the service (WatchdogSec=30 in the
// unit file). A process that is merely slow does not trigger a restart:
// the probe would have to fail three times in a row (30s / 10s) first.
// The goroutine runs until the process exits; there is nothing to stop.
func startWatchdogHeartbeat(db *sql.DB) {
	if os.Getenv("NOTIFY_SOCKET") == "" {
		return // not running under systemd; nothing to report to
	}
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			err := db.PingContext(ctx)
			cancel()
			if err == nil {
				notifyWatchdog()
			}
		}
	}()
}
