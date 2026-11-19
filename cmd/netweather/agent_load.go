package main

import (
	"fmt"
	"net"
	"os"
	"strconv"

	"github.com/Greg-2600/netweather/internal/agent"
	"github.com/Greg-2600/netweather/internal/config"
	"github.com/Greg-2600/netweather/internal/schedule"
)

// loadAgentTargets reads targets from a config file. It mirrors loadTargets
// without the store writes, since an agent has no local database.
func loadAgentTargets(path string) ([]agent.Target, error) {
	if path == "" {
		return nil, nil
	}
	f, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	out := make([]agent.Target, 0, len(f.Targets))
	for _, t := range f.Targets {
		var (
			url       string
			address   string
			resolvers []string
		)
		switch t.Probe {
		case "http":
			url = targetURL(t.Host, t.Port, t.Path)
		case "doh":
			url = dohEndpoint(t.Host, t.Port, t.URL)
		case "tls", "tcp", "tcpping", "dns", "ntp", "dot", "quic":
			address = net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
		case "icmp":
			address = t.Host
		default:
			return nil, fmt.Errorf("target %s: probe %q is not implemented yet", t.Host, t.Probe)
		}
		if t.Probe == "dns" {
			resolvers = append(resolvers, address)
			for _, extra := range t.Resolvers {
				if extra != address {
					resolvers = append(resolvers, extra)
				}
			}
		}
		expect, err := t.Expect.Classify()
		if err != nil {
			return nil, fmt.Errorf("target %s: %w", t.Host, err)
		}
		expect.ICMPAnchor = t.ICMPAnchor
		out = append(out, agent.Target{
			ID:           t.ID,
			Name:         t.Name,
			Host:         t.Host,
			URL:          url,
			Address:      address,
			Query:        t.QName,
			Resolvers:    resolvers,
			Instrument:   t.Probe,
			Vantage:      os.Getenv("NETWEATHER_VANTAGE"),
			Tier:         schedule.Tier(t.Tier),
			Interval:     t.Interval(),
			Expect:       expect,
			MaxRedirects: expect.MaxRedirects,
			ICMPAnchor:   t.ICMPAnchor,
		})
	}
	if outVantage := os.Getenv("NETWEATHER_VANTAGE"); outVantage == "" {
		for i := range out {
			if out[i].Vantage == "" {
				out[i].Vantage = "remote"
			}
		}
	}
	return out, nil
}
