package feeds_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Greg-2600/netweather/internal/feeds"
)

func awsStatusFixture(t *testing.T) *httptest.Server {
	t.Helper()

	const feed = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Amazon Web Services Service Status</title>
    <item>
      <title>Service disruption in EC2 (us-east-1)</title>
      <description>We are investigating increased error rates.</description>
      <pubDate>Tue, 17 Nov 2026 10:00:00 GMT</pubDate>
      <guid>https://status.aws.amazon.com/#ec2-1</guid>
      <link>https://status.aws.amazon.com/</link>
    </item>
    <item>
      <title>Increased error rates in S3 (eu-west-1)</title>
      <description>Some requests are failing.</description>
      <pubDate>Tue, 17 Nov 2026 09:00:00 GMT</pubDate>
      <guid>https://status.aws.amazon.com/#s3-1</guid>
    </item>
    <item>
      <title>Scheduled maintenance in RDS</title>
      <description>Planned work.</description>
      <pubDate>Tue, 17 Nov 2026 08:00:00 GMT</pubDate>
      <link>https://status.aws.amazon.com/#rds-1</link>
    </item>
    <item>
      <title>Service is operating normally: [RESOLVED] EC2</title>
      <description>All clear.</description>
      <pubDate>Tue, 17 Nov 2026 07:00:00 GMT</pubDate>
      <guid>https://status.aws.amazon.com/#resolved</guid>
    </item>
    <item>
      <title>No timestamp</title>
      <description>Cannot be placed on a timeline.</description>
    </item>
  </channel>
</rss>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); !strings.Contains(got, "xml") {
			t.Errorf("Accept = %q", got)
		}
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(feed))
	}))
	return srv
}

func TestAWSStatusFetchParsesIncidents(t *testing.T) {
	t.Parallel()

	srv := awsStatusFixture(t)
	defer srv.Close()

	aws := feeds.NewAWSStatus(srv.Client())
	aws.URL = srv.URL
	aws.Now = func() time.Time { return time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC) }

	events, err := aws.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("events = %d, want the three non-normal, timestamped items: %+v", len(events), events)
	}

	first := events[0]
	if first.Kind != "aws_status" || first.Severity != 1.0 {
		t.Fatalf("first = %+v", first)
	}
	if first.ExternalID != "https://status.aws.amazon.com/#ec2-1" {
		t.Fatalf("first id = %q", first.ExternalID)
	}
	if !first.StartedAt.Equal(time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("first start = %v", first.StartedAt)
	}
	if first.Payload == "" || !strings.Contains(first.Payload, "EC2") {
		t.Fatalf("payload = %q", first.Payload)
	}

	second := events[1]
	if second.Severity != 0.7 || !strings.Contains(second.ExternalID, "s3-1") {
		t.Fatalf("second = %+v", second)
	}

	third := events[2]
	if third.Severity != 0.2 || !strings.Contains(third.ExternalID, "rds-1") {
		t.Fatalf("third (link as id) = %+v", third)
	}

	for _, e := range events {
		if strings.Contains(strings.ToLower(e.Payload), "operating normally") {
			t.Fatalf("a normal status leaked into the events: %+v", e)
		}
	}
}

func TestAWSStatusRejectsBadResponses(t *testing.T) {
	t.Parallel()

	if _, err := feeds.NewAWSStatus(nil).Fetch(context.Background()); err == nil {
		t.Fatal("a nil client should be an error")
	}

	badStatus := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer badStatus.Close()
	aws := feeds.NewAWSStatus(badStatus.Client())
	aws.URL = badStatus.URL
	if _, err := aws.Fetch(context.Background()); err == nil {
		t.Fatal("a 500 should be an error")
	}

	badXML := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<rss><channel><item>"))
	}))
	defer badXML.Close()
	aws = feeds.NewAWSStatus(badXML.Client())
	aws.URL = badXML.URL
	if _, err := aws.Fetch(context.Background()); err == nil {
		t.Fatal("malformed XML should be an error")
	}
}
