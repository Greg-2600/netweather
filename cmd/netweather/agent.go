package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Greg-2600/netweather/internal/agent"
	"github.com/Greg-2600/netweather/internal/netx"
	"github.com/Greg-2600/netweather/internal/store"
)

// remoteSink posts a batch of probes to a primary server's vantage ingest
// endpoint. It is the agent side of federation-lite.
type remoteSink struct {
	server  string
	vantage string
	token   string
	client  *http.Client
}

func (r *remoteSink) Record(ctx context.Context, probes []store.Probe) error {
	if r.client == nil {
		r.client = &http.Client{Timeout: 30 * time.Second}
	}
	body, err := json.Marshal(map[string]any{"probes": probes})
	if err != nil {
		return fmt.Errorf("agent: marshal batch: %w", err)
	}
	url := strings.TrimRight(r.server, "/") + "/api/v1/vantages/" + r.vantage + "/reports"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("agent: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("agent: post batch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		return fmt.Errorf("agent: server returned %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

// agentCommand runs the probe loop as a remote vantage agent.
func agentCommand(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	server := fs.String("server", os.Getenv("NETWEATHER_SERVER_URL"), "primary server base URL")
	vantage := fs.String("vantage", os.Getenv("NETWEATHER_VANTAGE"), "vantage id to report under")
	token := fs.String("token", os.Getenv("NETWEATHER_AGENT_TOKEN"), "bearer token for the primary")
	configPath := fs.String("config", os.Getenv("NETWEATHER_CONFIG"), "target config file")
	interval := fs.Duration("interval", 30*time.Second, "reporting cadence")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	if *server == "" {
		return fmt.Errorf("agent: --server (or NETWEATHER_SERVER_URL) is required")
	}
	if *vantage == "" {
		return fmt.Errorf("agent: --vantage (or NETWEATHER_VANTAGE) is required")
	}

	targets, err := loadAgentTargets(*configPath)
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dialer := netx.New()
	instruments := newInstrumentsWithDialer(dialer, "netweather-agent")
	sink := &remoteSink{server: *server, vantage: *vantage, token: *token}
	ag := agent.New(targets, instruments, sink)
	ag.Logger = logger

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for {
		if _, err := ag.Step(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "agent: step: %v\n", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(*interval):
		}
	}
}
