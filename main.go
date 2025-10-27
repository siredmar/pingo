// cmd/pingo/main.go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/go-ping/ping"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Interval          time.Duration `yaml:"interval"`           // e.g. "10s"
	Timeout           time.Duration `yaml:"timeout"`            // per target "2s"
	Count             int           `yaml:"count"`              // echo packets per probe (>=1)
	FailureThreshold  int           `yaml:"failure_threshold"`  // consecutive fail → alert
	RecoveryThreshold int           `yaml:"recovery_threshold"` // consecutive success → recovery
	Targets           []Target      `yaml:"targets"`            // hosts to probe
	Signal            SignalConfig  `yaml:"signal"`
}

type Target struct {
	Host string `yaml:"host"`
	Desc string `yaml:"desc"`
}

// Allow either scalar string or mapping {host, desc}
func (t *Target) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		var host string
		if err := value.Decode(&host); err != nil {
			return err
		}
		t.Host = host
		return nil
	case yaml.MappingNode:
		type raw Target
		var r raw
		if err := value.Decode(&r); err != nil {
			return err
		}
		*t = Target(r)
		return nil
	default:
		return fmt.Errorf("invalid target entry")
	}
}

type SignalConfig struct {
	APIURL     string   `yaml:"api_url"`     // http://signal:8080
	FromNumber string   `yaml:"from_number"` // +49XXXXXXXXX
	Recipients []string `yaml:"recipients"`  // numbers or group IDs
	PrefixDown string   `yaml:"prefix_down"` // default: "⚠️ DOWN"
	PrefixUp   string   `yaml:"prefix_up"`   // default: "✅ UP"
}

type state struct {
	down          bool
	consecFail    int
	consecSuccess int
	lastChange    time.Time
}

type signalPayload struct {
	Number     string   `json:"number"`
	Recipients []string `json:"recipients"`
	Message    string   `json:"message"`
}

type pingStats struct{ Sent, Recv int }

func loadConfig() (*Config, error) {
	path := os.Getenv("CONFIG")
	if path == "" {
		path = "config.yaml"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	// defaults
	if c.Interval == 0 {
		c.Interval = 10 * time.Second
	}
	if c.Timeout == 0 {
		c.Timeout = 2 * time.Second
	}
	if c.Count <= 0 {
		c.Count = 3
	}
	if c.FailureThreshold <= 0 {
		c.FailureThreshold = 3
	}
	if c.RecoveryThreshold <= 0 {
		c.RecoveryThreshold = 3
	}
	if c.Signal.PrefixDown == "" {
		c.Signal.PrefixDown = "⚠️ DOWN"
	}
	if c.Signal.PrefixUp == "" {
		c.Signal.PrefixUp = "✅ UP"
	}
	// validation
	if c.Signal.APIURL == "" || c.Signal.FromNumber == "" || len(c.Signal.Recipients) == 0 {
		return nil, errors.New("signal.api_url, signal.from_number and signal.recipients are required")
	}
	if len(c.Targets) == 0 {
		return nil, errors.New("no targets configured")
	}
	for _, t := range c.Targets {
		if t.Host == "" {
			return nil, errors.New("target.host is required")
		}
	}
	return &c, nil
}

func label(t Target) string {
	if t.Desc != "" {
		return fmt.Sprintf("%s — %s", t.Desc, t.Host)
	}
	return t.Host
}

// pingOnceStats sends raw ICMP (requires CAP_NET_RAW or root) and returns stats.
// It treats "no packets received" as a clean error for the caller to count as failure.
func pingOnceStats(ctx context.Context, host string, count int, timeout time.Duration) (pingStats, error) {
	p := ping.New(host)
	p.Count = count
	p.Timeout = timeout
	p.SetPrivileged(true)

	errCh := make(chan error, 1)
	go func() { errCh <- p.Run() }()

	select {
	case <-ctx.Done():
		p.Stop()
		return pingStats{}, ctx.Err()
	case err := <-errCh:
		if err != nil {
			return pingStats{}, err
		}
		s := p.Statistics()
		return pingStats{Sent: s.PacketsSent, Recv: s.PacketsRecv}, nil
	}
}

func notifySignal(ctx context.Context, cfg SignalConfig, msg string) error {
	body := signalPayload{
		Number:     cfg.FromNumber,
		Recipients: cfg.Recipients,
		Message:    msg,
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.APIURL+"/v2/send", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	hc := &http.Client{Timeout: 10 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("signal send failed: http %d", resp.StatusCode)
	}
	return nil
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(2)
	}

	states := make(map[string]*state, len(cfg.Targets))
	for _, t := range cfg.Targets {
		states[t.Host] = &state{down: false, lastChange: time.Now()}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// OS signal handling
	sigc := make(chan os.Signal, 2)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-sigc
		fmt.Printf("received signal: %s — shutting down...\n", s)
		cancel()
		// force exit on second signal
		s = <-sigc
		fmt.Printf("received second signal: %s — forcing exit\n", s)
		os.Exit(1)
	}()

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	var wg sync.WaitGroup

	runProbe := func(ctx context.Context, tgt Target, st *state) {
		defer wg.Done()

		select {
		case <-ctx.Done():
			return
		default:
		}

		stats, err := pingOnceStats(ctx, tgt.Host, cfg.Count, cfg.Timeout)

		// If shutting down, ignore and don't touch state
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}

		lbl := label(tgt)

		if err != nil {
			// Real error (DNS, perms, etc.) — count as a failed probe attempt
			st.consecFail++
			st.consecSuccess = 0
			fmt.Printf("[%s] ping error: %v\n", lbl, err)
		} else if stats.Recv == 0 {
			// Clean run but no replies — treat as failure
			st.consecFail++
			st.consecSuccess = 0
			fmt.Printf("[%s] no replies (sent=%d)\n", lbl, stats.Sent)
		} else {
			// At least one reply — success
			st.consecSuccess++
			st.consecFail = 0
			fmt.Printf("[%s] ok (%d)\n", lbl, st.consecSuccess)
		}

		// DOWN transition
		if !st.down && st.consecFail >= cfg.FailureThreshold {
			st.down = true
			st.lastChange = time.Now()
			msg := fmt.Sprintf("%s: %s is unreachable (failed %d probes).",
				cfg.Signal.PrefixDown, lbl, st.consecFail)
			_ = notifySignal(ctx, cfg.Signal, msg)
			fmt.Println(msg)
		}

		// UP transition
		if st.down && st.consecSuccess >= cfg.RecoveryThreshold {
			st.down = false
			downtime := time.Since(st.lastChange).Truncate(time.Second)
			st.lastChange = time.Now()
			msg := fmt.Sprintf("%s: %s recovered after %s.",
				cfg.Signal.PrefixUp, lbl, downtime)
			_ = notifySignal(ctx, cfg.Signal, msg)
			fmt.Println(msg)
		}
	}

loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-ticker.C:
			for _, t := range cfg.Targets {
				wg.Add(1)
				go runProbe(ctx, t, states[t.Host])
			}
			// Wait for this batch to finish before scheduling next
			wg.Wait()
		}
	}

	// Final graceful wait for any stragglers (defensive)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		fmt.Println("shutdown timeout; exiting with probes still in flight")
	}
	fmt.Println("bye")
}
