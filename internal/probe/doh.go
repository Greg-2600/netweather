package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/Greg-2600/netweather/internal/netx"
)

// DefaultDoHMaxBytes bounds one JSON answer. A DNS response with an A record is
// a few hundred bytes; 64 KiB is generous and still refuses a page.
const DefaultDoHMaxBytes = 32 << 10

// DoH is the DNS-over-HTTPS instrument from DESIGN.md §5.1: one HTTPS GET to a
// fixed JSON endpoint asking for one A record. It answers "does this name
// resolve from outside my resolver?" without comparing resolvers to each other,
// which is the mistake CLASSIFIER.md §3.2 removed.
type DoH struct {
	Client    *http.Client
	MaxBytes  int64
	UserAgent string
}

// NewDoH builds the production instrument through the guarded dialer, so the
// endpoint cannot resolve to a private address and the port allowlist applies.
func NewDoH(dialer *netx.Dialer) *DoH {
	return NewDoHWithClient(&http.Client{
		Transport: Transport(dialer),
		Timeout:   DefaultTimeout,
		// A DoH endpoint does not redirect. Following one could move the query
		// to a host the operator did not configure.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	})
}

// NewDoHWithClient is the test seam: it uses the caller's client and transport.
// If the client has no redirect policy, the instrument's no-redirect rule is
// installed, so a caller cannot accidentally turn the endpoint into a redirect
// follower.
func NewDoHWithClient(client *http.Client) *DoH {
	if client != nil && client.CheckRedirect == nil {
		client.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}
	return &DoH{Client: client, MaxBytes: DefaultDoHMaxBytes}
}

type dohAnswer struct {
	Status int `json:"Status"`
	Answer []struct {
		Type int    `json:"type"`
		Data string `json:"data"`
	} `json:"Answer"`
}

// Do asks endpoint about qname. endpoint is the DoH JSON base
// ("https://cloudflare-dns.com/dns-query"); it is operator configuration, not
// discovered, and must be HTTPS: a plaintext answer is not evidence.
func (d *DoH) Do(ctx context.Context, endpoint, qname string) Result {
	res := Result{}
	start := time.Now()

	endpoint = strings.TrimSpace(endpoint)
	qname = strings.TrimSpace(strings.TrimSuffix(qname, "."))
	if endpoint == "" || qname == "" {
		res.ErrorClass = "doh_config"
		res.Err = fmt.Errorf("probe: doh needs an HTTPS endpoint and a qname")
		return res
	}

	u, err := url.Parse(endpoint)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Host == "" {
		res.ErrorClass = "doh_config"
		res.Err = fmt.Errorf("probe: doh endpoint %q is not an HTTPS URL", endpoint)
		return res
	}
	query := u.Query()
	query.Set("name", qname)
	query.Set("type", "A")
	u.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		res.ErrorClass = "doh_config"
		res.Err = err
		return res
	}
	ua := d.UserAgent
	if ua == "" {
		ua = DefaultUserAgent
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "application/dns-json")

	client := d.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	res.Latency = time.Since(start)
	if err != nil {
		res.ErrorClass = ClassifyError(err)
		if res.ErrorClass == "" {
			res.ErrorClass = "doh_error"
		}
		res.Err = err
		return res
	}
	defer func() { _ = resp.Body.Close() }()

	res.Status = resp.StatusCode
	if resp.StatusCode != http.StatusOK {
		res.ErrorClass = "doh_http"
		res.Err = fmt.Errorf("probe: doh endpoint returned HTTP %d", resp.StatusCode)
		return res
	}

	limit := d.MaxBytes
	if limit <= 0 {
		limit = DefaultDoHMaxBytes
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		res.ErrorClass = "doh_error"
		res.Err = err
		return res
	}

	var answer dohAnswer
	if err := json.Unmarshal(body, &answer); err != nil {
		res.ErrorClass = "doh_error"
		res.Err = fmt.Errorf("probe: doh answer is not JSON: %w", err)
		return res
	}

	switch answer.Status {
	case 0:
		// NOERROR; fall through to the answer section.
	case 3:
		res.ErrorClass = "doh_nxdomain"
		res.Err = fmt.Errorf("probe: %s does not exist", qname)
		return res
	default:
		res.ErrorClass = "doh_servfail"
		res.Err = fmt.Errorf("probe: doh rcode %d for %s", answer.Status, qname)
		return res
	}

	for _, record := range answer.Answer {
		if record.Type != 1 { // A
			continue
		}
		if addr, err := netip.ParseAddr(strings.TrimSpace(record.Data)); err == nil {
			res.Addresses = append(res.Addresses, addr.Unmap())
		}
	}
	if len(res.Addresses) == 0 {
		res.ErrorClass = "doh_no_answer"
		res.Err = fmt.Errorf("probe: doh returned no A record for %s", qname)
		return res
	}
	return res
}
