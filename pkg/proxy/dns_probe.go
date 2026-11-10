package proxy

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	dnslist "agy-patcher/dns"
)

const (
	// DefaultRepoCatalogURL points to the repository DNS provider catalog that can be refreshed without rebuilding.
	DefaultRepoCatalogURL = "https://raw.githubusercontent.com/CogniDevs/agy-patcher/main/dns/providers.json"
	// DefaultDNSAIEndpointsURL points to the live DNS-AI node manifest used as a best-effort IP refresh during catalog sync.
	DefaultDNSAIEndpointsURL = "https://raw.githubusercontent.com/confeden/DNS-AI/main/endpoints.json"
)

// DNSCatalog models dns/providers.json for embedded defaults and remote sync.
type DNSCatalog struct {
	Version   int               `json:"version"`
	Updated   string            `json:"updated"`
	FeedURL   string            `json:"feed_url,omitempty"`
	Providers []DNSProviderSpec `json:"providers"`
}

// CatalogSyncStats reports changes produced by SyncCatalogProviders.
type CatalogSyncStats struct {
	Added   int
	Updated int
	Removed int
}

// DNSProbeStatus classifies live verification outcomes for a DNS provider.
type DNSProbeStatus string

const (
	ProbeSub    DNSProbeStatus = "sub"
	ProbeDirect DNSProbeStatus = "direct"
	ProbeTLSErr DNSProbeStatus = "tls_err"
	ProbeErr    DNSProbeStatus = "err"
)

// DNSProbeHostResult captures resolution and TLS check details for a single gate hostname.
type DNSProbeHostResult struct {
	Host        string
	IPs         []string
	Substituted bool
	TLSOk       bool
	Err         string
}

// DNSProbeResult summarizes a live verification pass for one DNS provider.
type DNSProbeResult struct {
	Key       string
	Name      string
	Kind      string
	Target    string
	Status    DNSProbeStatus
	LatencyMs int64
	Summary   string
	Hosts     []DNSProbeHostResult
}

// ProbeOptions configures live DNS provider verification.
type ProbeOptions struct {
	Hosts          []string
	ReferenceAddrs []string
	Timeout        time.Duration
	DialTLSCheck   func(ctx context.Context, ip, serverName string) error
	Pool           *ResolverPool
}

// ProviderSyncKey returns a canonical identifier used to correlate providers across reordering and config updates.
func ProviderSyncKey(sp DNSProviderSpec) string {
	return strings.ToLower(strings.TrimSpace(sp.Kind) + "|" + strings.TrimSuffix(strings.TrimSpace(sp.Target), "/"))
}

// EmbeddedCatalog parses the repository DNS catalog compiled into the binary.
func EmbeddedCatalog() DNSCatalog {
	cat, err := ParseCatalog(dnslist.ProvidersJSON)
	if err != nil {
		return DNSCatalog{}
	}
	return cat
}

// ParseCatalog unmarshals and validates a repository providers.json manifest
// or a plain-text newline-delimited endpoint list from an external feed.
func ParseCatalog(raw []byte) (DNSCatalog, error) {
	trimmed := strings.TrimSpace(strings.TrimPrefix(string(raw), "\xef\xbb\xbf"))
	if trimmed == "" {
		return DNSCatalog{}, errors.New("dns catalog is empty")
	}

	var cat DNSCatalog
	seen := make(map[string]bool)
	var valid []DNSProviderSpec

	if strings.HasPrefix(trimmed, "{") {
		if err := json.Unmarshal([]byte(trimmed), &cat); err != nil {
			return DNSCatalog{}, fmt.Errorf("parse dns catalog: %w", err)
		}
		cat.FeedURL = strings.TrimSpace(cat.FeedURL)
		for _, p := range cat.Providers {
			normKind := strings.ToLower(strings.TrimSpace(p.Kind))
			if normKind != DNSKindDoH && normKind != DNSKindDoT && normKind != DNSKindUDP {
				continue
			}
			target := strings.TrimSpace(p.Target)
			if target == "" {
				continue
			}
			name := strings.TrimSpace(p.Name)
			if name == "" {
				name = target
			}
			spec := DNSProviderSpec{
				Name:        name,
				Kind:        normKind,
				Target:      target,
				FallbackIPs: append([]string(nil), p.FallbackIPs...),
				Enabled:     p.Enabled,
				Source:      DNSSourceCatalog,
			}
			key := ProviderSyncKey(spec)
			if seen[key] {
				continue
			}
			seen[key] = true
			valid = append(valid, spec)
		}
		if len(valid) == 0 && cat.FeedURL == "" {
			return DNSCatalog{}, errors.New("dns catalog contains no valid providers")
		}
		cat.Providers = valid
		return cat, nil
	}

	// Plain-text feed fallback: one entry per line (comments with #, ;, or // ignored).
	for _, line := range strings.Split(trimmed, "\n") {
		s := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if s == "" || strings.HasPrefix(s, "#") || strings.HasPrefix(s, ";") || strings.HasPrefix(s, "//") {
			continue
		}
		spec, err := ParseDNSProviderInput(s)
		if err != nil {
			continue
		}
		spec.Enabled = false
		spec.Source = DNSSourceCatalog
		key := ProviderSyncKey(spec)
		if seen[key] {
			continue
		}
		seen[key] = true
		valid = append(valid, spec)
	}
	if len(valid) == 0 {
		return DNSCatalog{}, errors.New("dns catalog contains no valid providers")
	}
	return DNSCatalog{Version: 1, Providers: valid}, nil
}

// FetchRepoCatalog downloads and parses the latest DNS catalog from GitHub (or a custom feed URL).
func FetchRepoCatalog(ctx context.Context, catalogURL, dnsAIEndpointsURL string) (DNSCatalog, error) {
	catalogURL = strings.TrimSpace(catalogURL)
	if catalogURL == "" {
		catalogURL = DefaultRepoCatalogURL
	}
	fetchCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	client := &http.Client{Timeout: 6 * time.Second}
	cat, err := fetchAndParseCatalog(fetchCtx, client, catalogURL)
	if err != nil {
		return DNSCatalog{}, err
	}

	// Optional 1-hop delegation when a catalog points feed_url to an external collector repo.
	if cat.FeedURL != "" && !strings.EqualFold(cat.FeedURL, catalogURL) {
		if subCat, subErr := fetchAndParseCatalog(fetchCtx, client, cat.FeedURL); subErr == nil && len(subCat.Providers) > 0 {
			cat = subCat
		} else if len(cat.Providers) == 0 {
			if subErr != nil {
				return DNSCatalog{}, subErr
			}
			return DNSCatalog{}, errors.New("delegated dns feed contains no valid providers")
		}
	}

	if dnsAIEndpointsURL == "" && strings.EqualFold(catalogURL, DefaultRepoCatalogURL) {
		dnsAIEndpointsURL = DefaultDNSAIEndpointsURL
	}
	if dnsAIEndpointsURL != "" {
		applyLiveDNSAINodes(fetchCtx, client, dnsAIEndpointsURL, &cat)
	}
	return cat, nil
}

func fetchAndParseCatalog(ctx context.Context, client *http.Client, targetURL string) (DNSCatalog, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return DNSCatalog{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return DNSCatalog{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return DNSCatalog{}, fmt.Errorf("catalog HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return DNSCatalog{}, err
	}
	return ParseCatalog(raw)
}

func applyLiveDNSAINodes(ctx context.Context, client *http.Client, endpointsURL string, cat *DNSCatalog) {
	subCtx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(subCtx, http.MethodGet, endpointsURL, nil)
	if err != nil {
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return
	}
	var ep struct {
		V4 []string `json:"v4"`
	}
	if json.Unmarshal(raw, &ep) != nil {
		return
	}
	var validV4 []string
	for _, s := range ep.V4 {
		if ip := net.ParseIP(strings.TrimSpace(s)); ip != nil && ip.To4() != nil {
			validV4 = append(validV4, ip.String())
		}
	}
	if len(validV4) == 0 {
		return
	}
	for i := range cat.Providers {
		if cat.Providers[i].Kind == DNSKindDoH && strings.EqualFold(cat.Providers[i].Target, DohURL) {
			cat.Providers[i].FallbackIPs = validV4
			break
		}
	}
}

// MergeCatalogProviders merges repository updates into existing providers while preserving user entries and Enabled choices.
func MergeCatalogProviders(existing, catalog []DNSProviderSpec) (merged []DNSProviderSpec, added int, updated int) {
	out, stats := SyncCatalogProviders(existing, nil, catalog)
	return out, stats.Added, stats.Updated
}

// SyncCatalogProviders synchronizes catalog entries against catalog while keeping all user-owned providers untouched.
// Retired entries marked with Source==DNSSourceCatalog that are absent from catalog are pruned, while user entries
// (Source==DNSSourceUser or present in userSaved) and untagged legacy entries are preserved intact.
func SyncCatalogProviders(existing, userSaved, catalog []DNSProviderSpec) (merged []DNSProviderSpec, stats CatalogSyncStats) {
	catByKey := make(map[string]DNSProviderSpec, len(catalog))
	for _, sp := range catalog {
		sp.Source = DNSSourceCatalog
		catByKey[ProviderSyncKey(sp)] = sp
	}

	userByKey := make(map[string]DNSProviderSpec, len(userSaved))
	for _, sp := range userSaved {
		sp.Source = DNSSourceUser
		userByKey[ProviderSyncKey(sp)] = sp
	}

	seen := make(map[string]bool, len(existing)+len(userSaved)+len(catalog))
	for _, cur := range existing {
		copySpec := DNSProviderSpec{
			Name:        cur.Name,
			Kind:        cur.Kind,
			Target:      cur.Target,
			FallbackIPs: append([]string(nil), cur.FallbackIPs...),
			Enabled:     cur.Enabled,
			Source:      cur.Source,
		}
		key := ProviderSyncKey(copySpec)
		if copySpec.Source == DNSSourceUser || userByKey[key].Target != "" {
			copySpec.Source = DNSSourceUser
			seen[key] = true
			merged = append(merged, copySpec)
			continue
		}
		if catEntry, ok := catByKey[key]; ok {
			if (copySpec.Source == DNSSourceCatalog && catEntry.Name != "" && copySpec.Name != catEntry.Name) ||
				!sameStringSlice(copySpec.FallbackIPs, catEntry.FallbackIPs) {
				if catEntry.Name != "" {
					copySpec.Name = catEntry.Name
				}
				copySpec.FallbackIPs = append([]string(nil), catEntry.FallbackIPs...)
				stats.Updated++
			}
			copySpec.Source = DNSSourceCatalog
			seen[key] = true
			merged = append(merged, copySpec)
			continue
		}
		if copySpec.Source == DNSSourceCatalog {
			stats.Removed++
			continue
		}
		// Legacy untagged entry (Source == "") not in catalog: preserve rather than drop user data.
		seen[key] = true
		merged = append(merged, copySpec)
	}

	for _, u := range userSaved {
		key := ProviderSyncKey(u)
		if seen[key] {
			continue
		}
		seen[key] = true
		merged = append(merged, DNSProviderSpec{
			Name:        u.Name,
			Kind:        u.Kind,
			Target:      u.Target,
			FallbackIPs: append([]string(nil), u.FallbackIPs...),
			Enabled:     u.Enabled,
			Source:      DNSSourceUser,
		})
	}

	for _, catEntry := range catalog {
		key := ProviderSyncKey(catEntry)
		if seen[key] {
			continue
		}
		seen[key] = true
		merged = append(merged, DNSProviderSpec{
			Name:        catEntry.Name,
			Kind:        catEntry.Kind,
			Target:      catEntry.Target,
			FallbackIPs: append([]string(nil), catEntry.FallbackIPs...),
			Enabled:     catEntry.Enabled,
			Source:      DNSSourceCatalog,
		})
		stats.Added++
	}
	return merged, stats
}

func sameStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ProbeProviders tests every provider in list against reference resolvers and verifies TLS reachability on substituted IPs.
func ProbeProviders(ctx context.Context, list []DNSProviderSpec, opts ProbeOptions) []DNSProbeResult {
	hosts := opts.Hosts
	if len(hosts) == 0 {
		hosts = []string{"daily-cloudcode-pa.googleapis.com", "cloudcode-pa.googleapis.com"}
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 4 * time.Second
	}
	pool := opts.Pool
	if pool == nil {
		pool = NewResolverPool("", "")
	}
	refAddrs := opts.ReferenceAddrs
	if refAddrs == nil {
		refAddrs = pool.ReferenceAddrs
	}
	tlsCheck := opts.DialTLSCheck
	if tlsCheck == nil {
		tlsCheck = defaultTLSProbeCheck
	}

	// Query reference IPs once per target host before probing candidates.
	refByHost := make(map[string]map[string]bool, len(hosts))
	var refMu sync.Mutex
	var refWG sync.WaitGroup
	for _, host := range hosts {
		refSet := make(map[string]bool)
		refByHost[host] = refSet
		for _, rAddr := range refAddrs {
			refWG.Add(1)
			go func(h, target string) {
				defer refWG.Done()
				rCtx, rCancel := context.WithTimeout(ctx, 1800*time.Millisecond)
				ans, err := pool.queryUDP(rCtx, target, h)
				rCancel()
				if err != nil {
					return
				}
				refMu.Lock()
				for _, ip := range ans.ips {
					if !dpiStubIPs[ip] {
						refByHost[h][ip] = true
					}
				}
				refMu.Unlock()
			}(host, rAddr)
		}
	}
	refWG.Wait()

	out := make([]DNSProbeResult, len(list))
	var wg sync.WaitGroup
	for idx, sp := range list {
		wg.Add(1)
		go func(i int, spec DNSProviderSpec) {
			defer wg.Done()
			started := time.Now()
			res := DNSProbeResult{
				Key:    ProviderSyncKey(spec),
				Name:   spec.Name,
				Kind:   spec.Kind,
				Target: spec.Target,
				Status: ProbeErr,
			}
			var (
				subOKHosts  []string
				firstTLSErr string
				firstErr    string
				anyDirect   bool
			)
			for _, h := range hosts {
				pCtx, cancel := context.WithTimeout(ctx, timeout)
				ans, err := pool.queryProvider(pCtx, spec, h)
				cancel()
				hr := DNSProbeHostResult{Host: h}
				if err != nil || len(ans.ips) == 0 {
					if err != nil {
						hr.Err = err.Error()
					} else {
						hr.Err = "no A records"
					}
					if firstErr == "" {
						firstErr = hr.Err
					}
					res.Hosts = append(res.Hosts, hr)
					continue
				}
				var cleanIPs []string
				for _, ip := range ans.ips {
					if !dpiStubIPs[ip] {
						cleanIPs = append(cleanIPs, ip)
					}
				}
				if len(cleanIPs) == 0 {
					hr.Err = "ISP DPI stub IP"
					if firstErr == "" {
						firstErr = hr.Err
					}
					res.Hosts = append(res.Hosts, hr)
					continue
				}
				hr.IPs = cleanIPs
				refSet := refByHost[h]
				hr.Substituted = len(refSet) > 0 && !overlaps(cleanIPs, refSet)
				if hr.Substituted {
					var lastTLSErr error
					limit := len(cleanIPs)
					if limit > 2 {
						limit = 2
					}
					for _, ip := range cleanIPs[:limit] {
						tCtx, tCancel := context.WithTimeout(ctx, 2500*time.Millisecond)
						tErr := tlsCheck(tCtx, ip, h)
						tCancel()
						if tErr == nil {
							hr.TLSOk = true
							break
						}
						lastTLSErr = tErr
					}
					if hr.TLSOk {
						subOKHosts = append(subOKHosts, shortGateLabel(h))
					} else if lastTLSErr != nil {
						hr.Err = lastTLSErr.Error()
						if firstTLSErr == "" {
							firstTLSErr = hr.Err
						}
					}
				} else {
					anyDirect = true
				}
				res.Hosts = append(res.Hosts, hr)
			}
			res.LatencyMs = time.Since(started).Milliseconds()
			switch {
			case len(subOKHosts) > 0:
				res.Status = ProbeSub
				res.Summary = strings.Join(subOKHosts, ",")
			case firstTLSErr != "":
				res.Status = ProbeTLSErr
				res.Summary = firstTLSErr
			case anyDirect:
				res.Status = ProbeDirect
			default:
				res.Status = ProbeErr
				res.Summary = firstErr
			}
			out[i] = res
		}(idx, sp)
	}
	wg.Wait()
	return out
}

func shortGateLabel(host string) string {
	host = strings.TrimSuffix(host, ".googleapis.com")
	if host == "daily-cloudcode-pa" {
		return "daily"
	}
	if host == "cloudcode-pa" {
		return "cloudcode"
	}
	return host
}

func defaultTLSProbeCheck(ctx context.Context, ip, serverName string) error {
	d := &net.Dialer{Timeout: 2500 * time.Millisecond}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip, "443"))
	if err != nil {
		return err
	}
	tlsConn := tls.Client(conn, &tls.Config{
		ServerName: serverName,
		MinVersion: tls.VersionTLS12,
	})
	if dl, ok := ctx.Deadline(); ok {
		_ = tlsConn.SetDeadline(dl)
	} else {
		_ = tlsConn.SetDeadline(time.Now().Add(2500 * time.Millisecond))
	}
	err = tlsConn.Handshake()
	_ = tlsConn.Close()
	return err
}
