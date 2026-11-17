package enrich

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// PeerDB reads internet-exchange membership from PeeringDB.
//
// It is a label, never a monitoring signal (DESIGN.md §8.5). One ASN can sit
// behind hundreds of targets, so the answer is cached per ASN for the lifetime
// of the process: a fleet of 750 targets costs one request per distinct ASN,
// not one per target.
type PeerDB struct {
	// BaseURL is the API root, injectable for tests.
	BaseURL string
	// Client performs the requests. Nil is a configuration error at call time.
	Client *http.Client
	// MaxNames caps the named exchanges, because the drawer row is a label and
	// a wall of names is not more informative than the first few.
	MaxNames int
	// MaxBytes caps the response before parsing.
	MaxBytes int64

	mu    sync.Mutex
	cache map[int][]string
}

// DefaultPeerDBBase is PeeringDB's public API root.
const DefaultPeerDBBase = "https://www.peeringdb.com/api"

// NewPeerDB returns a client with the production defaults.
func NewPeerDB(client *http.Client) *PeerDB {
	return &PeerDB{
		BaseURL:  DefaultPeerDBBase,
		Client:   client,
		MaxNames: 6,
		MaxBytes: 2 << 20,
		cache:    make(map[int][]string),
	}
}

// IXNames returns the internet exchanges the ASN is present at. An empty list
// is a real answer ("not at a known IX"), not an error, and a transport failure
// is an error so the caller can keep the previous labels.
func (p *PeerDB) IXNames(ctx context.Context, asn int) ([]string, error) {
	if asn <= 0 {
		return nil, fmt.Errorf("enrich: peeringdb needs a positive ASN")
	}
	if p.Client == nil {
		return nil, fmt.Errorf("enrich: peeringdb client is not configured")
	}

	if names, ok := p.cached(asn); ok {
		return names, nil
	}

	base := strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	if base == "" {
		base = DefaultPeerDBBase
	}
	endpoint := base + "/netixlan?asn=" + strconv.Itoa(asn)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("enrich: peeringdb request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "netweather (+https://github.com/Greg-2600/netweather)")

	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("enrich: peeringdb AS%d: %w", asn, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("enrich: peeringdb AS%d returned %s", asn, resp.Status)
	}

	maxBytes := p.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 2 << 20
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	if err != nil {
		return nil, fmt.Errorf("enrich: peeringdb AS%d body: %w", asn, err)
	}

	names, err := parsePeerDBIX(body, p.MaxNames)
	if err != nil {
		return nil, fmt.Errorf("enrich: peeringdb AS%d payload: %w", asn, err)
	}

	p.store(asn, names)
	return names, nil
}

// parsePeerDBIX extracts the distinct exchange names from a netixlan response.
func parsePeerDBIX(body []byte, max int) ([]string, error) {
	var envelope struct {
		Data []struct {
			Name string `json:"name"`
			IXID int    `json:"ix_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}

	seen := make(map[string]struct{}, len(envelope.Data))
	names := make([]string, 0, len(envelope.Data))
	for _, row := range envelope.Data {
		name := strings.TrimSpace(row.Name)
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	sort.Strings(names)

	if max <= 0 {
		max = 6
	}
	if len(names) > max {
		names = names[:max]
	}
	return names, nil
}

func (p *PeerDB) cached(asn int) ([]string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	names, ok := p.cache[asn]
	if !ok {
		return nil, false
	}
	return append([]string(nil), names...), true
}

func (p *PeerDB) store(asn int, names []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cache == nil {
		p.cache = make(map[int][]string)
	}
	p.cache[asn] = append([]string(nil), names...)
}
