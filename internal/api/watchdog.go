package api

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func (s *Server) watchdogLoop() {
	if os.Getenv("RC_INTERNAL_WATCHDOG") == "0" {
		return
	}
	interval := durationEnv("RC_WATCHDOG_INTERVAL", 30*time.Second)
	timeout := durationEnv("RC_WATCHDOG_TIMEOUT", 5*time.Second)
	grace := durationEnv("RC_WATCHDOG_GRACE", 15*time.Second)
	maxFailures := intEnv("RC_WATCHDOG_MAX_FAILURES", 2)
	exitCode := intEnv("RC_WATCHDOG_EXIT_CODE", 70)
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if grace < 0 {
		grace = 0
	}
	if maxFailures <= 0 {
		maxFailures = 2
	}

	timer := time.NewTimer(grace)
	defer timer.Stop()
	failures := 0
	for {
		select {
		case <-s.pushStop:
			return
		case <-timer.C:
			if err := s.selfHealthCheck(timeout); err != nil {
				failures++
				fmt.Fprintf(os.Stderr, "remote-agent internal watchdog: health failed failures=%d err=%v\n", failures, err)
				if failures >= maxFailures {
					fmt.Fprintf(os.Stderr, "remote-agent internal watchdog: exiting for supervisor restart code=%d\n", exitCode)
					os.Exit(exitCode)
				}
			} else if failures != 0 {
				failures = 0
				fmt.Fprintln(os.Stderr, "remote-agent internal watchdog: recovered")
			}
			timer.Reset(interval)
		}
	}
}

func (s *Server) selfHealthCheck(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	client := &http.Client{Timeout: timeout}
	url := "http://" + s.cfg.Host + ":" + strconv.Itoa(s.cfg.Port) + "/healthz"
	if s.cfg.UDS != "" {
		uds := s.cfg.UDS
		if st, err := os.Stat(uds); err != nil {
			return fmt.Errorf("uds stat %s: %w", uds, err)
		} else if st.IsDir() || (st.Mode()&os.ModeSocket) == 0 {
			return fmt.Errorf("uds path is not a socket: %s", uds)
		}
		client.Transport = &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", uds)
			},
		}
		url = "http://unix/healthz"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Remote-Agent-Client-Kind", "watchdog")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("healthz HTTP %d", resp.StatusCode)
	}
	return nil
}

func durationEnv(name string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return fallback
	}
	return d
}

func intEnv(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return v
}
