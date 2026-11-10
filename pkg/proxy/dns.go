package proxy

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	DohURL     = "https://dns.dns-ai.ru/dns-query"
	MalwDohURL = "https://dns.malw.link/dns-query"

	DNSKindDoH = "doh"
	DNSKindDoT = "dot"
	DNSKindUDP = "udp"

	DNSSourceCatalog = "catalog"
	DNSSourceUser    = "user"

	DNSModeAuto   = "race"
	DNSModeStrict = "strict"

	raceBudget  = 1500 * time.Millisecond
	dohGrace    = 850 * time.Millisecond
	minCacheTTL = 30 * time.Second
	maxCacheTTL = 5 * time.Minute
)

// DNSProviderSpec defines an upstream DoH, DoT, or UDP resolver in priority order.
type DNSProviderSpec struct {
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`                   // "doh", "dot", or "udp"
	Target      string   `json:"target"`                 // DoH https:// URL, DoT tls://host[:port], or UDP host:port
	FallbackIPs []string `json:"fallback_ips,omitempty"` // DoH/DoT bootstrap IPs or secondary UDP addresses
	Enabled     bool     `json:"enabled"`
	Source      string   `json:"source,omitempty"` // "catalog" or "user"
}

// DefaultDNSProviders returns the baseline repository DNS provider list embedded in dns/providers.json.
func DefaultDNSProviders() []DNSProviderSpec {
	cat := EmbeddedCatalog()
	if len(cat.Providers) > 0 {
		return cat.Providers
	}
	return []DNSProviderSpec{
		{
			Name:        "dns-ai.ru",
			Kind:        DNSKindDoH,
			Target:      DohURL,
			FallbackIPs: []string{"194.87.196.13", "45.151.102.219"},
			Enabled:     true,
			Source:      DNSSourceCatalog,
		},
		{
			Name:        "eu.geohide.ru",
			Kind:        DNSKindUDP,
			Target:      "31.25.239.132:53",
			FallbackIPs: []string{"143.20.64.192:53", "5.175.161.3:53", "37.221.93.141:53"},
			Enabled:     true,
			Source:      DNSSourceCatalog,
		},
	}
}

// NormalizeDNSMode normalizes lookup mode strings to DNSModeAuto ("race") or DNSModeStrict ("strict").
func NormalizeDNSMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case DNSModeStrict, "sequential", "order":
		return DNSModeStrict
	default:
		return DNSModeAuto
	}
}

func (sp DNSProviderSpec) displayName() string {
	name := strings.TrimSpace(sp.Name)
	if name == "" {
		name = sp.Target
	}
	switch strings.ToLower(strings.TrimSpace(sp.Kind)) {
	case DNSKindDoH:
		if !strings.Contains(strings.ToLower(name), "(doh)") {
			return name + " (DoH)"
		}
	case DNSKindDoT:
		if !strings.Contains(strings.ToLower(name), "(dot)") {
			return name + " (DoT)"
		}
	}
	return name
}

func (sp DNSProviderSpec) udpTargets() []string {
	var out []string
	if sp.Target != "" {
		out = append(out, sp.Target)
	}
	out = append(out, sp.FallbackIPs...)
	return out
}

// FormatDNSProviderInput renders sp into a compact editable string for UI prompts.
func FormatDNSProviderInput(sp DNSProviderSpec) string {
	parts := []string{}
	if strings.TrimSpace(sp.Name) != "" {
		parts = append(parts, strings.TrimSpace(sp.Name))
	}
	if strings.EqualFold(sp.Kind, DNSKindUDP) {
		ips := sp.udpTargets()
		parts = append(parts, strings.Join(ips, ","))
		return strings.Join(parts, " | ")
	}
	parts = append(parts, sp.Target)
	if len(sp.FallbackIPs) > 0 {
		parts = append(parts, strings.Join(sp.FallbackIPs, ","))
	}
	return strings.Join(parts, " | ")
}

// ParseDNSProviderInput parses user input into a validated DoH, DoT, or UDP DNSProviderSpec.
// Accepted formats:
//   - "https://host/dns-query" (DoH)
//   - "tls://host[:port]" or bare domain "dns.example.com" (DoT)
//   - "1.2.3.4", "1.2.3.4:53", "host:53", or "1.2.3.4,5.6.7.8" (UDP)
//   - "Name | target | fallback_ip1,fallback_ip2"
func ParseDNSProviderInput(raw string) (DNSProviderSpec, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return DNSProviderSpec{}, fmt.Errorf("empty DNS server entry")
	}

	var name, targetPart, extraPart string
	if strings.Contains(trimmed, "|") {
		segs := strings.Split(trimmed, "|")
		if len(segs) > 3 {
			return DNSProviderSpec{}, fmt.Errorf("too many '|' parts")
		}
		if len(segs) == 2 {
			left := strings.TrimSpace(segs[0])
			right := strings.TrimSpace(segs[1])
			lowLeft := strings.ToLower(left)
			if strings.HasPrefix(lowLeft, "https://") || strings.HasPrefix(lowLeft, "tls://") {
				targetPart = left
				extraPart = right
			} else {
				name = left
				targetPart = right
			}
		} else {
			name = strings.TrimSpace(segs[0])
			targetPart = strings.TrimSpace(segs[1])
			extraPart = strings.TrimSpace(segs[2])
		}
	} else if eq := strings.Index(trimmed, "="); eq > 0 &&
		!strings.HasPrefix(strings.ToLower(trimmed), "https://") &&
		!strings.HasPrefix(strings.ToLower(trimmed), "tls://") {
		name = strings.TrimSpace(trimmed[:eq])
		targetPart = strings.TrimSpace(trimmed[eq+1:])
	} else {
		targetPart = trimmed
	}

	if targetPart == "" {
		return DNSProviderSpec{}, fmt.Errorf("missing DNS URL or IP address")
	}

	parseFallbackIPs := func(rawList string) ([]string, error) {
		var fallbacks []string
		for _, tok := range strings.Split(rawList, ",") {
			ipStr := strings.TrimSpace(tok)
			if ipStr == "" {
				continue
			}
			ip := net.ParseIP(ipStr)
			if ip == nil {
				return nil, fmt.Errorf("invalid bootstrap IP %q", ipStr)
			}
			fallbacks = append(fallbacks, ip.String())
		}
		return fallbacks, nil
	}

	low := strings.ToLower(targetPart)
	if strings.HasPrefix(low, "tls://") || isBareHostname(targetPart) {
		rawTLS := targetPart
		if !strings.HasPrefix(low, "tls://") {
			rawTLS = "tls://" + targetPart
		}
		u, err := url.Parse(rawTLS)
		if err != nil || u.Hostname() == "" {
			return DNSProviderSpec{}, fmt.Errorf("DoT endpoint must be a valid tls://host[:port]")
		}
		normTarget := "tls://" + u.Hostname()
		if port := u.Port(); port != "" {
			pNum, pErr := strconv.Atoi(port)
			if pErr != nil || pNum < 1 || pNum > 65535 {
				return DNSProviderSpec{}, fmt.Errorf("invalid DoT port %q", port)
			}
			if port != "853" {
				normTarget = fmt.Sprintf("tls://%s:%d", u.Hostname(), pNum)
			}
		}
		if name == "" {
			name = u.Hostname()
		}
		fallbacks, err := parseFallbackIPs(extraPart)
		if err != nil {
			return DNSProviderSpec{}, err
		}
		return DNSProviderSpec{
			Name:        name,
			Kind:        DNSKindDoT,
			Target:      normTarget,
			FallbackIPs: fallbacks,
			Enabled:     true,
		}, nil
	}

	if strings.HasPrefix(low, "https://") || strings.Contains(low, "://") {
		u, err := url.Parse(targetPart)
		if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Hostname() == "" {
			return DNSProviderSpec{}, fmt.Errorf("DoH endpoint must be a valid https:// URL")
		}
		if name == "" {
			name = u.Hostname()
		}
		fallbacks, err := parseFallbackIPs(extraPart)
		if err != nil {
			return DNSProviderSpec{}, err
		}
		return DNSProviderSpec{
			Name:        name,
			Kind:        DNSKindDoH,
			Target:      targetPart,
			FallbackIPs: fallbacks,
			Enabled:     true,
		}, nil
	}

	var combined []string
	for _, src := range []string{targetPart, extraPart} {
		if src == "" {
			continue
		}
		for _, tok := range strings.Split(src, ",") {
			normAddr, err := normalizeUDPAddr(tok)
			if err != nil {
				return DNSProviderSpec{}, err
			}
			combined = append(combined, normAddr)
		}
	}
	if len(combined) == 0 {
		return DNSProviderSpec{}, fmt.Errorf("missing UDP DNS address")
	}
	if name == "" {
		host, _, _ := net.SplitHostPort(combined[0])
		name = host
	}
	return DNSProviderSpec{
		Name:        name,
		Kind:        DNSKindUDP,
		Target:      combined[0],
		FallbackIPs: combined[1:],
		Enabled:     true,
	}, nil
}

func isBareHostname(s string) bool {
	if s == "" || net.ParseIP(s) != nil {
		return false
	}
	if strings.ContainsAny(s, ":/\\, \t") {
		return false
	}
	return strings.Contains(s, ".")
}

func normalizeUDPAddr(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("empty UDP address")
	}
	if ip := net.ParseIP(s); ip != nil {
		return net.JoinHostPort(ip.String(), "53"), nil
	}
	host, portStr, err := net.SplitHostPort(s)
	if err != nil || host == "" {
		return "", fmt.Errorf("invalid UDP DNS address %q (expected IP or IP:port)", raw)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("invalid port in UDP DNS address %q", raw)
	}
	if ip := net.ParseIP(host); ip != nil {
		return net.JoinHostPort(ip.String(), strconv.Itoa(port)), nil
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

var (
	// Active DNS-AI IPv4 nodes (185.251.90.181 was retired in endpoints.json serial 7).
	dnsAiFallbackAddrs = []string{
		"194.87.196.13",
		"45.151.102.219",
	}

	defaultSecondaryDoHs = []dohProvider(nil)

	defaultUDPProviders = []udpProvider{
		{name: "eu.geohide.ru", addrs: []string{"31.25.239.132:53", "143.20.64.192:53", "5.175.161.3:53", "37.221.93.141:53"}},
	}

	defaultReferenceAddrs = []string{"8.8.8.8:53", "1.1.1.1:53"}

	dpiStubIPs = map[string]bool{
		"8.6.112.0": true,
		"8.47.69.0": true,
	}
)

type dohProvider struct {
	name        string
	url         string
	fallbackIPs []string
}

type udpProvider struct {
	name  string
	addrs []string
}

type dnsCacheEntry struct {
	ips         []string
	substituted bool
	expiresAt   time.Time
}

type dnsAnswer struct {
	ips []string
	ttl uint32
}

// ResolveDetail describes how a gate hostname was resolved.
type ResolveDetail struct {
	Host        string
	Provider    string
	IPs         []string
	Substituted bool
	Cached      bool
	Latency     time.Duration
}

type inflightResolve struct {
	done        chan struct{}
	ips         []string
	substituted bool
	provider    string
	err         error
}

type provResult struct {
	priority int
	isDoH    bool
	provider string
	ans      dnsAnswer
	err      error
}

// ResolverPool races or sequentially queries split-DNS/DoH providers against reference resolvers
// to prefer substituted egress IPs for region-gated hosts.
type ResolverPool struct {
	DohURL         string
	DohFallbackIPs []string
	SecondaryDoHs  []dohProvider
	UDPProviders   []udpProvider
	ReferenceAddrs []string
	Client         *http.Client

	mu           sync.RWMutex
	mode         string
	providers    []DNSProviderSpec
	providersSet bool
	cache        map[string]dnsCacheEntry
	inflight     map[string]*inflightResolve
	benched      map[string]time.Time
	selfMu       sync.RWMutex
	selfAddrs    map[string]bool
	udpTxID      atomic.Uint32
	OnResolve    func(ResolveDetail)
}

// UpdateDNS atomically applies a user-configured DNS mode and ordered provider list, clearing cached entries.
func (p *ResolverPool) UpdateDNS(mode string, providers []DNSProviderSpec) {
	if p == nil {
		return
	}
	copied := make([]DNSProviderSpec, len(providers))
	for i, sp := range providers {
		sp.FallbackIPs = append([]string(nil), sp.FallbackIPs...)
		copied[i] = sp
	}
	normMode := NormalizeDNSMode(mode)

	p.mu.Lock()
	p.mode = normMode
	p.providers = copied
	p.providersSet = true
	p.cache = make(map[string]dnsCacheEntry)
	p.benched = make(map[string]time.Time)
	p.mu.Unlock()
}

func (p *ResolverPool) snapshotDNSConfig() (string, []DNSProviderSpec) {
	p.mu.RLock()
	mode := p.mode
	custom := p.providersSet
	provs := append([]DNSProviderSpec(nil), p.providers...)
	p.mu.RUnlock()

	if mode == "" {
		mode = DNSModeAuto
	}
	if custom {
		var enabled []DNSProviderSpec
		for _, sp := range provs {
			if sp.Enabled && sp.Target != "" {
				enabled = append(enabled, sp)
			}
		}
		return mode, enabled
	}

	var list []DNSProviderSpec
	if p.DohURL != "" {
		name := "dns-ai.ru"
		if p.DohURL != DohURL {
			if u, err := url.Parse(p.DohURL); err == nil && u.Hostname() != "" {
				name = u.Hostname()
			}
		}
		list = append(list, DNSProviderSpec{
			Name:        name,
			Kind:        DNSKindDoH,
			Target:      p.DohURL,
			FallbackIPs: append([]string(nil), p.DohFallbackIPs...),
			Enabled:     true,
		})
	}
	for _, dp := range p.SecondaryDoHs {
		list = append(list, DNSProviderSpec{
			Name:        dp.name,
			Kind:        DNSKindDoH,
			Target:      dp.url,
			FallbackIPs: append([]string(nil), dp.fallbackIPs...),
			Enabled:     true,
		})
	}
	for _, up := range p.UDPProviders {
		if len(up.addrs) == 0 {
			continue
		}
		list = append(list, DNSProviderSpec{
			Name:        up.name,
			Kind:        DNSKindUDP,
			Target:      up.addrs[0],
			FallbackIPs: append([]string(nil), up.addrs[1:]...),
			Enabled:     true,
		})
	}
	return mode, list
}

func (p *ResolverPool) lookupDoHFallbacks(host string) []string {
	p.mu.RLock()
	custom := p.providersSet
	provs := p.providers
	p.mu.RUnlock()

	if custom {
		for _, sp := range provs {
			if !sp.Enabled || !strings.EqualFold(sp.Kind, DNSKindDoH) || len(sp.FallbackIPs) == 0 {
				continue
			}
			if su, err := url.Parse(sp.Target); err == nil && strings.EqualFold(host, su.Hostname()) {
				return append([]string(nil), sp.FallbackIPs...)
			}
		}
		return nil
	}

	if pu, err := url.Parse(p.DohURL); err == nil && strings.EqualFold(host, pu.Hostname()) && len(p.DohFallbackIPs) > 0 {
		return p.DohFallbackIPs
	}
	for _, sp := range p.SecondaryDoHs {
		if su, pErr := url.Parse(sp.url); pErr == nil && strings.EqualFold(host, su.Hostname()) && len(sp.fallbackIPs) > 0 {
			return sp.fallbackIPs
		}
	}
	return nil
}

// RecordSelfAddr marks addr as a local bypass proxy listener so DoH requests never loop through it.
func (p *ResolverPool) RecordSelfAddr(addr string) {
	if p == nil || addr == "" {
		return
	}
	p.selfMu.Lock()
	if p.selfAddrs == nil {
		p.selfAddrs = make(map[string]bool)
	}
	p.selfAddrs[strings.ToLower(addr)] = true
	p.selfMu.Unlock()
}

func (p *ResolverPool) isSelfAddr(host string) bool {
	if p == nil || host == "" {
		return false
	}
	p.selfMu.RLock()
	ok := p.selfAddrs[strings.ToLower(host)]
	p.selfMu.RUnlock()
	return ok
}

// NewResolverPool creates a ResolverPool configured with default DoH + UDP providers.
func NewResolverPool(listenAddr, dohURL string) *ResolverPool {
	isDefault := dohURL == "" || dohURL == DohURL
	if dohURL == "" {
		dohURL = DohURL
	}

	var secondaryDoHs []dohProvider
	if isDefault {
		secondaryDoHs = append([]dohProvider(nil), defaultSecondaryDoHs...)
	}

	selfAddrs := map[string]bool{
		strings.ToLower(ListenAddr): true,
	}
	if listenAddr != "" {
		selfAddrs[strings.ToLower(listenAddr)] = true
	}
	for _, fb := range FallbackAddrs {
		selfAddrs[strings.ToLower(fb)] = true
	}

	pool := &ResolverPool{
		DohURL:         dohURL,
		DohFallbackIPs: append([]string(nil), dnsAiFallbackAddrs...),
		SecondaryDoHs:  secondaryDoHs,
		UDPProviders:   defaultUDPProviders,
		ReferenceAddrs: defaultReferenceAddrs,
		cache:          make(map[string]dnsCacheEntry),
		inflight:       make(map[string]*inflightResolve),
		benched:        make(map[string]time.Time),
		selfAddrs:      selfAddrs,
	}

	baseDialer := &net.Dialer{Timeout: 2 * time.Second}
	transport := &http.Transport{
		ForceAttemptHTTP2: true,
		IdleConnTimeout:   20 * time.Second,
		TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12},
		Proxy: func(req *http.Request) (*url.URL, error) {
			pURL, err := http.ProxyFromEnvironment(req)
			if err != nil || pURL == nil {
				return pURL, err
			}
			if pool.isSelfAddr(pURL.Host) {
				return nil, nil
			}
			return pURL, nil
		},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err == nil && net.ParseIP(host) == nil {
				if rawFallbacks := pool.lookupDoHFallbacks(host); len(rawFallbacks) > 0 {
					candidates := pool.orderFallbackList(rawFallbacks)
					for _, ip := range candidates {
						subCtx, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
						conn, dialErr := baseDialer.DialContext(subCtx, network, net.JoinHostPort(ip, port))
						cancel()
						if dialErr == nil {
							return conn, nil
						}
						pool.benchFallbackIP(ip, 3*time.Minute)
					}
				}
			}
			return baseDialer.DialContext(ctx, network, addr)
		},
	}

	pool.Client = &http.Client{
		Timeout:   5 * time.Second,
		Transport: transport,
	}
	return pool
}

// Invalidate removes any cached DNS entry for host so the next resolution re-queries upstream.
func (p *ResolverPool) Invalidate(host string) {
	hostname := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if hostname == "" {
		return
	}
	p.mu.Lock()
	delete(p.cache, hostname)
	p.mu.Unlock()
}

// ResolveGateHost returns candidate IP addresses for host, preferring substituted answers.
// Concurrent lookups for the same uncached host share a single in-flight DNS race.
func (p *ResolverPool) ResolveGateHost(ctx context.Context, host string) ([]string, bool, error) {
	hostname := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if hostname == "" {
		return nil, false, fmt.Errorf("empty host")
	}
	if ip := net.ParseIP(hostname); ip != nil {
		return []string{ip.String()}, false, nil
	}

	p.mu.Lock()
	if entry, ok := p.cache[hostname]; ok && time.Now().Before(entry.expiresAt) {
		ips := append([]string(nil), entry.ips...)
		sub := entry.substituted
		p.mu.Unlock()
		return ips, sub, nil
	}
	if p.inflight == nil {
		p.inflight = make(map[string]*inflightResolve)
	}
	if call, ok := p.inflight[hostname]; ok {
		p.mu.Unlock()
		select {
		case <-call.done:
			return append([]string(nil), call.ips...), call.substituted, call.err
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
	}
	call := &inflightResolve{done: make(chan struct{})}
	p.inflight[hostname] = call
	p.mu.Unlock()

	// Run the DNS race decoupled from a single caller's cancellation so other
	// waiting callers don't fail if the first caller disconnects early.
	go func() {
		started := time.Now()
		ips, sub, prov, err := p.raceAndCache(hostname)
		p.mu.Lock()
		delete(p.inflight, hostname)
		call.ips = ips
		call.substituted = sub
		call.provider = prov
		call.err = err
		close(call.done)
		p.mu.Unlock()
		if err == nil && p.OnResolve != nil {
			p.OnResolve(ResolveDetail{
				Host:        hostname,
				Provider:    prov,
				IPs:         append([]string(nil), ips...),
				Substituted: sub,
				Latency:     time.Since(started),
			})
		}
	}()

	select {
	case <-call.done:
		return append([]string(nil), call.ips...), call.substituted, call.err
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}
}

func (p *ResolverPool) orderFallbackList(addrs []string) []string {
	n := len(addrs)
	if n == 0 {
		return nil
	}
	now := time.Now()

	p.mu.RLock()
	healthy := make([]string, 0, n)
	benched := make([]string, 0, n)
	for _, ip := range addrs {
		if until, ok := p.benched[ip]; ok && now.Before(until) {
			benched = append(benched, ip)
		} else {
			healthy = append(healthy, ip)
		}
	}
	p.mu.RUnlock()

	return append(healthy, benched...)
}

func (p *ResolverPool) benchFallbackIP(ip string, dur time.Duration) {
	p.mu.Lock()
	if p.benched == nil {
		p.benched = make(map[string]time.Time)
	}
	p.benched[ip] = time.Now().Add(dur)
	p.mu.Unlock()
}

func (p *ResolverPool) raceAndCache(hostname string) ([]string, bool, string, error) {
	started := time.Now()
	mode, enabled := p.snapshotDNSConfig()

	raceCtx, cancel := context.WithTimeout(context.Background(), raceBudget)
	defer cancel()

	refCh := make(chan map[string]bool, 1)
	go func() {
		refSet := make(map[string]bool)
		if len(p.ReferenceAddrs) == 0 || len(enabled) == 0 {
			refCh <- refSet
			return
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		for _, addr := range p.ReferenceAddrs {
			wg.Add(1)
			go func(target string) {
				defer wg.Done()
				if ans, err := p.queryUDP(raceCtx, target, hostname); err == nil {
					mu.Lock()
					for _, ip := range ans.ips {
						if !dpiStubIPs[ip] {
							refSet[ip] = true
						}
					}
					mu.Unlock()
				}
			}(addr)
		}
		wg.Wait()
		refCh <- refSet
	}()

	var (
		chosen      []string
		ttl         uint32
		winner      string
		substituted bool
	)

	if len(enabled) > 0 && mode == DNSModeStrict {
		var refSet map[string]bool
		for _, sp := range enabled {
			stepCtx, stepCancel := context.WithTimeout(context.Background(), raceBudget)
			ans, err := p.queryProvider(stepCtx, sp, hostname)
			stepCancel()
			if err != nil || len(ans.ips) == 0 {
				continue
			}
			if refSet == nil && len(p.ReferenceAddrs) > 0 {
				select {
				case rs := <-refCh:
					refSet = rs
				case <-time.After(350 * time.Millisecond):
				}
			}
			chosen = ans.ips
			ttl = ans.ttl
			winner = sp.displayName()
			substituted = len(refSet) > 0 && !overlaps(chosen, refSet)
			break
		}
	} else if len(enabled) > 0 {
		provCount := len(enabled)
		provCh := make(chan provResult, provCount)
		totalDoH := 0
		for idx, sp := range enabled {
			isEncrypted := strings.EqualFold(sp.Kind, DNSKindDoH) || strings.EqualFold(sp.Kind, DNSKindDoT)
			if isEncrypted {
				totalDoH++
			}
			go func(prio int, spec DNSProviderSpec, enc bool) {
				ans, err := p.queryProvider(raceCtx, spec, hostname)
				provCh <- provResult{priority: prio, isDoH: enc, provider: spec.displayName(), ans: ans, err: err}
			}(idx, sp, isEncrypted)
		}

		var results []provResult
		var refSet map[string]bool
		refDone := false
		received := 0

		var graceCh <-chan time.Time
		var graceTimer *time.Timer
		defer func() {
			if graceTimer != nil {
				graceTimer.Stop()
			}
		}()

		for received < provCount || !refDone {
			select {
			case r := <-provCh:
				results = append(results, r)
				received++
			case rs := <-refCh:
				refSet = rs
				refDone = true
			case <-graceCh:
				received = provCount
				refDone = true
				continue
			}

			if refDone {
				if totalDoH > 0 {
					allDoHDone := countDoHDone(results) >= totalDoH
					if allDoHDone && hasSubstitutedDoH(results, -1, refSet) {
						break
					}
					if provCount > 1 && hasSubstitutedDoH(results, 0, refSet) && graceTimer == nil {
						waitLeft := dohGrace - time.Since(started)
						if waitLeft <= 0 {
							break
						}
						graceTimer = time.NewTimer(waitLeft)
						graceCh = graceTimer.C
					}
				} else if provCount > 1 && hasSubstitutedAny(results, 0, refSet) && graceTimer == nil {
					waitLeft := dohGrace - time.Since(started)
					if waitLeft <= 0 {
						break
					}
					graceTimer = time.NewTimer(waitLeft)
					graceCh = graceTimer.C
				}
			}
		}

		var bestSub *provResult
		var bestAny *provResult
		for i := range results {
			r := &results[i]
			if r.err != nil || len(r.ans.ips) == 0 {
				continue
			}
			if bestAny == nil || r.priority < bestAny.priority {
				bestAny = r
			}
			if len(refSet) > 0 && !overlaps(r.ans.ips, refSet) {
				if bestSub == nil || r.priority < bestSub.priority {
					bestSub = r
				}
			}
		}

		switch {
		case bestSub != nil:
			chosen, winner, ttl = mergeSubstitutedResults(results, refSet, bestSub)
			substituted = true
		case bestAny != nil:
			chosen = bestAny.ans.ips
			ttl = bestAny.ans.ttl
			winner = bestAny.provider
		}
	}

	if len(chosen) == 0 {
		sysCtx, sysCancel := context.WithTimeout(context.Background(), 2*time.Second)
		addrs, err := net.DefaultResolver.LookupIP(sysCtx, "ip4", hostname)
		sysCancel()
		if err != nil || len(addrs) == 0 {
			return nil, false, "", fmt.Errorf("all DNS/DoH resolvers failed for %s", hostname)
		}
		for _, ip := range addrs {
			chosen = append(chosen, ip.String())
		}
		ttl = 60
		winner = "system"
	}

	ttlDur := time.Duration(ttl) * time.Second
	if ttlDur < minCacheTTL {
		ttlDur = minCacheTTL
	} else if ttlDur > maxCacheTTL {
		ttlDur = maxCacheTTL
	}

	p.mu.Lock()
	if p.cache == nil {
		p.cache = make(map[string]dnsCacheEntry)
	}
	p.cache[hostname] = dnsCacheEntry{
		ips:         append([]string(nil), chosen...),
		substituted: substituted,
		expiresAt:   time.Now().Add(ttlDur),
	}
	p.mu.Unlock()

	return chosen, substituted, winner, nil
}

func mergeSubstitutedResults(results []provResult, refSet map[string]bool, primary *provResult) ([]string, string, uint32) {
	seen := make(map[string]bool, 8)
	var ips []string
	var names []string
	minTTL := primary.ans.ttl

	addIPs := func(list []string) bool {
		added := false
		for _, ip := range list {
			if !seen[ip] {
				seen[ip] = true
				ips = append(ips, ip)
				added = true
			}
		}
		return added
	}

	addIPs(primary.ans.ips)
	names = append(names, primary.provider)

	// Append unique substituted IPs from other providers in priority order for multi-relay failover.
	for prio := 0; prio <= len(results)+4; prio++ {
		for i := range results {
			r := &results[i]
			if r == primary || r.priority != prio || r.err != nil || len(r.ans.ips) == 0 {
				continue
			}
			if overlaps(r.ans.ips, refSet) {
				continue
			}
			if addIPs(r.ans.ips) {
				names = append(names, r.provider)
				if r.ans.ttl > 0 && (minTTL == 0 || r.ans.ttl < minTTL) {
					minTTL = r.ans.ttl
				}
			}
		}
	}
	return ips, strings.Join(names, " + "), minTTL
}

func countDoHDone(results []provResult) int {
	n := 0
	for i := range results {
		if results[i].isDoH {
			n++
		}
	}
	return n
}

func hasSubstitutedDoH(results []provResult, maxPrio int, refSet map[string]bool) bool {
	if len(refSet) == 0 {
		return false
	}
	for i := range results {
		r := &results[i]
		if !r.isDoH {
			continue
		}
		if (maxPrio < 0 || r.priority <= maxPrio) && r.err == nil && len(r.ans.ips) > 0 && !overlaps(r.ans.ips, refSet) {
			return true
		}
	}
	return false
}

func hasSubstitutedAny(results []provResult, maxPrio int, refSet map[string]bool) bool {
	if len(refSet) == 0 {
		return false
	}
	for i := range results {
		r := &results[i]
		if (maxPrio < 0 || r.priority <= maxPrio) && r.err == nil && len(r.ans.ips) > 0 && !overlaps(r.ans.ips, refSet) {
			return true
		}
	}
	return false
}

func overlaps(ips []string, set map[string]bool) bool {
	for _, ip := range ips {
		if set[ip] {
			return true
		}
	}
	return false
}

func (p *ResolverPool) queryDoHEndpoint(ctx context.Context, endpointURL, hostname string) (dnsAnswer, error) {
	query, err := buildDNSQuery(hostname, 1, 0)
	if err != nil {
		return dnsAnswer{}, err
	}

	u, err := url.Parse(endpointURL)
	if err != nil {
		return dnsAnswer{}, fmt.Errorf("invalid DoH URL: %w", err)
	}
	q := u.Query()
	q.Set("dns", base64.RawURLEncoding.EncodeToString(query))
	u.RawQuery = q.Encode()

	var mu sync.Mutex
	var remoteIP string
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			if info.Conn != nil && info.Conn.RemoteAddr() != nil {
				if host, _, splitErr := net.SplitHostPort(info.Conn.RemoteAddr().String()); splitErr == nil {
					mu.Lock()
					remoteIP = host
					mu.Unlock()
				}
			}
		},
	}

	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodGet, u.String(), nil)
	if err != nil {
		return dnsAnswer{}, err
	}
	req.Header.Set("Accept", "application/dns-message")

	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}

	markFailedNode := func() {
		mu.Lock()
		ip := remoteIP
		mu.Unlock()
		if ip != "" {
			p.benchFallbackIP(ip, 3*time.Minute)
			if tr, ok := client.Transport.(*http.Transport); ok {
				tr.CloseIdleConnections()
			}
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		markFailedNode()
		return dnsAnswer{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode >= 500 {
			markFailedNode()
		}
		return dnsAnswer{}, fmt.Errorf("DoH returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		markFailedNode()
		return dnsAnswer{}, err
	}

	return parseDNSRecords(body)
}

func (p *ResolverPool) queryProvider(ctx context.Context, sp DNSProviderSpec, hostname string) (dnsAnswer, error) {
	switch strings.ToLower(strings.TrimSpace(sp.Kind)) {
	case DNSKindDoH:
		return p.queryDoHEndpoint(ctx, sp.Target, hostname)
	case DNSKindDoT:
		return p.queryDoTEndpoint(ctx, sp.Target, sp.FallbackIPs, hostname)
	default:
		return p.queryUDPList(ctx, sp.udpTargets(), hostname)
	}
}

func (p *ResolverPool) queryDoTEndpoint(ctx context.Context, rawTarget string, fallbackIPs []string, hostname string) (dnsAnswer, error) {
	u, err := url.Parse(rawTarget)
	if err != nil || u.Hostname() == "" {
		return dnsAnswer{}, fmt.Errorf("invalid DoT target %q", rawTarget)
	}
	sniHost := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "853"
	}

	var dialTargets []string
	for _, fb := range p.orderFallbackList(fallbackIPs) {
		if _, _, splitErr := net.SplitHostPort(fb); splitErr == nil {
			dialTargets = append(dialTargets, fb)
		} else {
			dialTargets = append(dialTargets, net.JoinHostPort(fb, port))
		}
	}
	dialTargets = append(dialTargets, net.JoinHostPort(sniHost, port))

	txID := uint16(p.udpTxID.Add(1))
	wire, err := buildDNSQuery(hostname, 1, txID)
	if err != nil {
		return dnsAnswer{}, err
	}
	frame := make([]byte, 2+len(wire))
	binary.BigEndian.PutUint16(frame[:2], uint16(len(wire)))
	copy(frame[2:], wire)

	dialer := &net.Dialer{Timeout: 2500 * time.Millisecond}
	tlsCfg := &tls.Config{
		ServerName: sniHost,
		MinVersion: tls.VersionTLS12,
	}

	var lastErr error
	for _, addr := range dialTargets {
		rawConn, dialErr := dialer.DialContext(ctx, "tcp", addr)
		if dialErr != nil {
			lastErr = dialErr
			continue
		}
		conn := tls.Client(rawConn, tlsCfg)
		if dl, ok := ctx.Deadline(); ok {
			_ = conn.SetDeadline(dl)
		} else {
			_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		}
		if hsErr := conn.HandshakeContext(ctx); hsErr != nil {
			_ = conn.Close()
			lastErr = hsErr
			continue
		}
		if _, wErr := conn.Write(frame); wErr != nil {
			_ = conn.Close()
			lastErr = wErr
			continue
		}
		var lenBuf [2]byte
		if _, rErr := io.ReadFull(conn, lenBuf[:]); rErr != nil {
			_ = conn.Close()
			lastErr = rErr
			continue
		}
		msgLen := int(binary.BigEndian.Uint16(lenBuf[:]))
		if msgLen < 12 || msgLen > 64*1024 {
			_ = conn.Close()
			lastErr = fmt.Errorf("invalid DoT response length %d", msgLen)
			continue
		}
		body := make([]byte, msgLen)
		if _, rErr := io.ReadFull(conn, body); rErr != nil {
			_ = conn.Close()
			lastErr = rErr
			continue
		}
		_ = conn.Close()
		if binary.BigEndian.Uint16(body[:2]) != txID {
			lastErr = fmt.Errorf("DoT transaction ID mismatch")
			continue
		}
		return parseDNSRecords(body)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no DoT endpoints configured")
	}
	return dnsAnswer{}, lastErr
}

func (p *ResolverPool) queryUDPList(ctx context.Context, addrs []string, hostname string) (dnsAnswer, error) {
	var lastErr error
	for _, addr := range addrs {
		ans, err := p.queryUDP(ctx, addr, hostname)
		if err == nil && len(ans.ips) > 0 {
			return ans, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no UDP servers configured")
	}
	return dnsAnswer{}, lastErr
}

func (p *ResolverPool) queryUDP(ctx context.Context, serverAddr, hostname string) (dnsAnswer, error) {
	txID := uint16(p.udpTxID.Add(1))
	query, err := buildDNSQuery(hostname, 1, txID)
	if err != nil {
		return dnsAnswer{}, err
	}

	dialer := &net.Dialer{Timeout: 800 * time.Millisecond}
	conn, err := dialer.DialContext(ctx, "udp", serverAddr)
	if err != nil {
		return dnsAnswer{}, err
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(900 * time.Millisecond))
	}

	if _, err := conn.Write(query); err != nil {
		return dnsAnswer{}, err
	}

	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		return dnsAnswer{}, err
	}
	if n < 2 || binary.BigEndian.Uint16(buf[:2]) != txID {
		return dnsAnswer{}, fmt.Errorf("DNS transaction ID mismatch")
	}
	return parseDNSRecords(buf[:n])
}

func buildDNSQuery(domain string, qtype uint16, txID uint16) ([]byte, error) {
	domain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
	if domain == "" || len(domain) > 253 {
		return nil, fmt.Errorf("invalid domain name %q", domain)
	}

	labels := strings.Split(domain, ".")
	buf := make([]byte, 12, 12+len(domain)+6)
	binary.BigEndian.PutUint16(buf[0:2], txID)
	buf[2] = 0x01 // RD=1
	buf[5] = 0x01 // QDCOUNT=1

	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 {
			return nil, fmt.Errorf("invalid DNS label %q in %q", label, domain)
		}
		buf = append(buf, byte(len(label)))
		buf = append(buf, label...)
	}
	buf = append(buf, 0x00)
	buf = binary.BigEndian.AppendUint16(buf, qtype)
	buf = binary.BigEndian.AppendUint16(buf, 1) // QCLASS IN
	return buf, nil
}

func parseDNSRecords(body []byte) (dnsAnswer, error) {
	if len(body) < 12 {
		return dnsAnswer{}, fmt.Errorf("invalid DNS response: too short (%d bytes)", len(body))
	}

	rcode := body[3] & 0x0F
	if rcode != 0 {
		return dnsAnswer{}, fmt.Errorf("DNS query failed with RCODE=%d", rcode)
	}

	qdCount := int(binary.BigEndian.Uint16(body[4:6]))
	anCount := int(binary.BigEndian.Uint16(body[6:8]))

	offset := 12
	for i := 0; i < qdCount; i++ {
		var err error
		offset, err = skipDNSName(body, offset)
		if err != nil {
			return dnsAnswer{}, fmt.Errorf("malformed question section: %w", err)
		}
		if offset+4 > len(body) {
			return dnsAnswer{}, fmt.Errorf("truncated question record")
		}
		offset += 4
	}

	var ipv4s []string
	var ipv6s []string
	var minV4TTL uint32
	var minV6TTL uint32

	for i := 0; i < anCount; i++ {
		var err error
		offset, err = skipDNSName(body, offset)
		if err != nil {
			return dnsAnswer{}, fmt.Errorf("malformed answer name: %w", err)
		}
		if offset+10 > len(body) {
			return dnsAnswer{}, fmt.Errorf("truncated answer header")
		}

		rrType := binary.BigEndian.Uint16(body[offset : offset+2])
		rrClass := binary.BigEndian.Uint16(body[offset+2 : offset+4])
		rrTTL := binary.BigEndian.Uint32(body[offset+4 : offset+8])
		rdLen := int(binary.BigEndian.Uint16(body[offset+8 : offset+10]))
		offset += 10

		if offset+rdLen > len(body) {
			return dnsAnswer{}, fmt.Errorf("truncated answer rdata")
		}

		if rrClass == 1 {
			if rrType == 1 && rdLen == 4 {
				ipv4s = append(ipv4s, net.IP(body[offset:offset+4]).String())
				if minV4TTL == 0 || rrTTL < minV4TTL {
					minV4TTL = rrTTL
				}
			} else if rrType == 28 && rdLen == 16 {
				ipv6s = append(ipv6s, net.IP(body[offset:offset+16]).String())
				if minV6TTL == 0 || rrTTL < minV6TTL {
					minV6TTL = rrTTL
				}
			}
		}

		offset += rdLen
	}

	if len(ipv4s) > 0 {
		return dnsAnswer{ips: ipv4s, ttl: minV4TTL}, nil
	}
	if len(ipv6s) > 0 {
		return dnsAnswer{ips: ipv6s, ttl: minV6TTL}, nil
	}
	return dnsAnswer{}, fmt.Errorf("no A/AAAA record found in DNS response")
}

func skipDNSName(msg []byte, offset int) (int, error) {
	for steps := 0; steps < 128 && offset < len(msg); steps++ {
		b := msg[offset]
		if b == 0 {
			return offset + 1, nil
		}
		if b&0xC0 == 0xC0 {
			if offset+2 > len(msg) {
				return 0, fmt.Errorf("truncated compression pointer")
			}
			return offset + 2, nil
		}
		if b&0xC0 != 0 {
			return 0, fmt.Errorf("invalid DNS label prefix 0x%02x", b)
		}
		offset += 1 + int(b)
		if offset > len(msg) {
			return 0, fmt.Errorf("truncated label")
		}
	}
	return 0, fmt.Errorf("unterminated DNS name")
}
