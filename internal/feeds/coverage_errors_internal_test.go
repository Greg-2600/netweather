package feeds //nolint:testpackage // white-box tests exercise injectable error paths without the network

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFeedFetchersRejectBadConfiguration(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	if _, err := (&Atlas{}).Fetch(ctx); err == nil {
		t.Error("Atlas accepted a nil client")
	}
	noMeasurements := NewAtlas(http.DefaultClient, "key", nil)
	if _, err := noMeasurements.Fetch(ctx); err == nil {
		t.Error("Atlas accepted an empty measurement list")
	}
	for _, endpoint := range []string{"%", "ftp://example.com", "http:///path"} {
		a := NewAtlas(http.DefaultClient, "key", []int{1})
		a.Endpoint = endpoint
		if _, err := a.Fetch(ctx); err == nil {
			t.Errorf("Atlas accepted endpoint %q", endpoint)
		}
	}

	if _, err := (&RISLive{}).Fetch(ctx); err == nil {
		t.Error("RISLive accepted a nil client")
	}
	noASNs := NewRISLive(http.DefaultClient, nil)
	if _, err := noASNs.Fetch(ctx); err == nil {
		t.Error("RISLive accepted an empty ASN list")
	}
	for _, endpoint := range []string{"%", "ftp://example.com", "http:///path"} {
		r := NewRISLive(http.DefaultClient, []int{1})
		r.Endpoint = endpoint
		if _, err := r.Fetch(ctx); err == nil {
			t.Errorf("RISLive accepted endpoint %q", endpoint)
		}
	}

	ioda := NewIODA(http.DefaultClient)
	ioda.Endpoint = "%"
	if _, err := ioda.Fetch(ctx); err == nil {
		t.Error("IODA accepted an unparseable endpoint")
	}
	ioda = NewIODA(http.DefaultClient)
	ioda.SummaryEndpoint = "%"
	if _, err := ioda.FetchLevels(ctx); err == nil {
		t.Error("IODA accepted an unparseable summary endpoint")
	}

	if _, err := (&RSSStatus{}).Fetch(ctx); err == nil {
		t.Error("RSSStatus accepted a nil client")
	}
	rss := NewAWSStatus(http.DefaultClient)
	rss.URL = "://bad"
	if _, err := rss.Fetch(ctx); err == nil {
		t.Error("RSSStatus accepted an unparseable URL")
	}

	gcp := NewGCPStatus(http.DefaultClient)
	gcp.URL = "://bad"
	if _, err := gcp.Fetch(ctx); err == nil {
		t.Error("GCPStatus accepted an unparseable URL")
	}

	if _, _, err := statuspageSummaryURL("http://%"); err == nil {
		t.Error("Statuspage accepted an unparseable URL")
	}
}

func TestFeedFetchersUseProductionDefaultsWithoutExplicitOptions(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rssAWS := &RSSStatus{Client: failingClient()}
	if _, err := rssAWS.Fetch(ctx); err == nil {
		t.Error("RSSStatus with default kind and URL should fail at the transport")
	}
	rssAzure := &RSSStatus{Kind: "azure_status", Client: failingClient()}
	if _, err := rssAzure.Fetch(ctx); err == nil {
		t.Error("RSSStatus with the Azure default URL should fail at the transport")
	}
	rssWhitespace := &RSSStatus{Kind: "  ", Client: failingClient()}
	if _, err := rssWhitespace.Fetch(ctx); err == nil {
		t.Error("RSSStatus with a blank kind should fail at the transport")
	}

	checks := []struct {
		name string
		run  func() error
	}{
		{"atlas", func() error {
			_, err := (&Atlas{Client: failingClient(), Measurements: []int{1}}).Fetch(ctx)
			return err
		}},
		{"rislive", func() error {
			_, err := (&RISLive{Client: failingClient(), ASNs: []int{1}}).Fetch(ctx)
			return err
		}},
		{"ioda", func() error {
			_, err := (&IODA{Client: failingClient()}).Fetch(ctx)
			return err
		}},
		{"ioda levels", func() error {
			_, err := (&IODA{Client: failingClient()}).FetchLevels(ctx)
			return err
		}},
		{"gcp", func() error {
			_, err := (&GCPStatus{Client: failingClient()}).Fetch(ctx)
			return err
		}},
		{"radar", func() error {
			_, err := (&Radar{Client: failingClient(), Token: "token"}).Fetch(ctx)
			return err
		}},
		{"statuspage", func() error {
			_, err := (&Statuspage{Client: failingClient(), URLs: []string{"https://status.example.com"}}).Fetch(ctx)
			return err
		}},
	}
	for _, tc := range checks {
		if err := tc.run(); err == nil {
			t.Errorf("%s: expected a transport error with default options", tc.name)
		}
	}
}

func TestFeedFetchersSurfaceUpstreamFailures(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	atlasStatus := NewAtlas(srv.Client(), "key", []int{1})
	atlasStatus.Endpoint = srv.URL
	risStatus := NewRISLive(srv.Client(), []int{1})
	risStatus.Endpoint = srv.URL
	iodaStatus := NewIODA(srv.Client())
	iodaStatus.Endpoint = srv.URL
	iodaLevelStatus := NewIODA(srv.Client())
	iodaLevelStatus.SummaryEndpoint = srv.URL
	rssStatus := NewAWSStatus(srv.Client())
	rssStatus.URL = srv.URL
	gcpStatus := NewGCPStatus(srv.Client())
	gcpStatus.URL = srv.URL

	atlasTransport := NewAtlas(failingClient(), "key", []int{1})
	risTransport := NewRISLive(failingClient(), []int{1})
	iodaTransport := NewIODA(failingClient())
	iodaLevelTransport := NewIODA(failingClient())
	rssTransport := NewAWSStatus(failingClient())
	gcpTransport := NewGCPStatus(failingClient())
	spTransport := NewStatuspage(failingClient(), []string{"https://status.example.com"})

	checks := []struct {
		name string
		run  func() error
	}{
		{"atlas status", func() error { _, err := atlasStatus.Fetch(ctx); return err }},
		{"ris status", func() error { _, err := risStatus.Fetch(ctx); return err }},
		{"ioda status", func() error { _, err := iodaStatus.Fetch(ctx); return err }},
		{"ioda level status", func() error { _, err := iodaLevelStatus.FetchLevels(ctx); return err }},
		{"rss status", func() error { _, err := rssStatus.Fetch(ctx); return err }},
		{"gcp status", func() error { _, err := gcpStatus.Fetch(ctx); return err }},
		{"atlas transport", func() error { _, err := atlasTransport.Fetch(ctx); return err }},
		{"ris transport", func() error { _, err := risTransport.Fetch(ctx); return err }},
		{"ioda transport", func() error { _, err := iodaTransport.Fetch(ctx); return err }},
		{"ioda level transport", func() error { _, err := iodaLevelTransport.FetchLevels(ctx); return err }},
		{"rss transport", func() error { _, err := rssTransport.Fetch(ctx); return err }},
		{"gcp transport", func() error { _, err := gcpTransport.Fetch(ctx); return err }},
		{"statuspage transport", func() error { _, err := spTransport.Fetch(ctx); return err }},
	}
	for _, tc := range checks {
		if err := tc.run(); err == nil {
			t.Errorf("%s: expected an error", tc.name)
		}
	}
}

func TestAtlasUsesDefaultsAndDerivesMissingResultFields(t *testing.T) {
	t.Parallel()

	const payload = `[
	  {"from":42,"dst_addr":"192.0.2.1","result":[{"rtt":0,"err":"timeout"}]},
	  {"timestamp":1795000000,"prb_id":7,"result":[{"rtt":12.5}]},
	  {"timestamp":1795000100,"result":[]}
	]`
	var mu sync.Mutex
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		query = r.URL.RawQuery
		mu.Unlock()
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()

	a := NewAtlas(srv.Client(), "", []int{99})
	a.Endpoint = srv.URL
	a.Now = coverageNow
	a.Lookback = 0
	a.MaxBytes = 0

	events, err := a.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want the two rows with probe results", len(events))
	}
	if events[0].Severity != 1 || !strings.HasPrefix(events[0].ExternalID, "atlas:99:") {
		t.Errorf("failed probe = %+v", events[0])
	}
	if !strings.Contains(events[0].Payload, `"rtt_ms":0`) || !strings.Contains(events[0].Payload, `"loss":1`) {
		t.Errorf("failed probe payload = %s", events[0].Payload)
	}
	if !events[0].StartedAt.Equal(coverageNow()) {
		t.Errorf("missing timestamp should fall back to now, got %v", events[0].StartedAt)
	}
	if events[1].Severity != 0 || !strings.Contains(events[1].Payload, `"rtt_ms":12.5`) {
		t.Errorf("successful probe = %+v", events[1])
	}

	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(query, "start=") || !strings.Contains(query, "stop=") {
		t.Errorf("query %q missing the default lookback window", query)
	}
	if strings.Contains(query, "key=") {
		t.Errorf("query %q unexpectedly carries a key", query)
	}
}

func TestRSSDateFormatsAndNormalStatusFilter(t *testing.T) {
	t.Parallel()

	const feed = `<?xml version="1.0" encoding="UTF-8"?>
	<rss version="2.0"><channel>
	  <item><title>Service unavailable</title>
	    <pubDate>Tue, 17 Nov 2026 10:00:00 +0000</pubDate><guid>a</guid></item>
	  <item><title>Slow performance</title>
	    <pubDate>Tue, 7 Nov 2026 09:00:00 GMT</pubDate><guid>b</guid></item>
	  <item><title>Planned advisory</title>
	    <pubDate>Tue, 17 Nov 2026 08:00:00 GMT</pubDate><guid>c</guid></item>
	  <item><title>No issues reported</title>
	    <pubDate>Tue, 17 Nov 2026 07:00:00 GMT</pubDate><guid>d</guid></item>
	</channel></rss>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(feed))
	}))
	defer srv.Close()

	rss := NewAWSStatus(srv.Client())
	rss.URL = srv.URL
	rss.Now = coverageNow

	events, err := rss.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("events = %d, want three (the no-issues item is dropped): %+v", len(events), events)
	}
	wantIDs := []string{"a", "b", "c"}
	wantSeverity := []float64{1.0, 0.5, 0.2}
	wantStarts := []time.Time{
		time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC),
	}
	for i, event := range events {
		if event.ExternalID != wantIDs[i] || event.Severity != wantSeverity[i] {
			t.Errorf("event[%d] = %+v, want id %q severity %v", i, event, wantIDs[i], wantSeverity[i])
		}
		if !event.StartedAt.Equal(wantStarts[i]) {
			t.Errorf("event[%d] start = %v, want %v", i, event.StartedAt, wantStarts[i])
		}
	}
}
