package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func buildTestDNSResponse(q []byte, ips ...[4]byte) []byte {
	resp := append([]byte(nil), q...)
	resp[2] = 0x81
	resp[3] = 0x80
	binary.BigEndian.PutUint16(resp[6:8], uint16(len(ips)))
	for _, ip := range ips {
		resp = append(resp, 0xC0, 0x0C)
		resp = binary.BigEndian.AppendUint16(resp, 1)  // A
		resp = binary.BigEndian.AppendUint16(resp, 1)  // IN
		resp = binary.BigEndian.AppendUint32(resp, 60) // TTL
		resp = binary.BigEndian.AppendUint16(resp, 4)
		resp = append(resp, ip[:]...)
	}
	return resp
}

func TestBuildAndParseDNSWire(t *testing.T) {
	q, err := buildDNSQuery("cloudcode-pa.googleapis.com", 1, 0)
	if err != nil {
		t.Fatalf("buildDNSQuery failed: %v", err)
	}

	resp := make([]byte, len(q))
	copy(resp, q)
	resp[2] = 0x81
	resp[3] = 0x80
	binary.BigEndian.PutUint16(resp[6:8], 2)

	// CNAME
	resp = append(resp, 0xC0, 0x0C)
	resp = binary.BigEndian.AppendUint16(resp, 5)
	resp = binary.BigEndian.AppendUint16(resp, 1)
	resp = binary.BigEndian.AppendUint32(resp, 60)
	cnameTarget := []byte{3, 'f', 'o', 'o', 3, 'b', 'a', 'r', 0}
	resp = binary.BigEndian.AppendUint16(resp, uint16(len(cnameTarget)))
	resp = append(resp, cnameTarget...)

	// A record
	resp = append(resp, cnameTarget...)
	resp = binary.BigEndian.AppendUint16(resp, 1)
	resp = binary.BigEndian.AppendUint16(resp, 1)
	resp = binary.BigEndian.AppendUint32(resp, 120)
	resp = binary.BigEndian.AppendUint16(resp, 4)
	resp = append(resp, 203, 0, 113, 42)

	ans, err := parseDNSRecords(resp)
	if err != nil {
		t.Fatalf("parseDNSRecords failed: %v", err)
	}
	if len(ans.ips) != 1 || ans.ips[0] != "203.0.113.42" || ans.ttl != 120 {
		t.Fatalf("got ips=%v ttl=%d, want [203.0.113.42] / 120", ans.ips, ans.ttl)
	}
}

func TestResolverPoolPrefersSubstitutedProviderOverPassthrough(t *testing.T) {
	// Reference UDP DNS server returns genuine 142.250.1.1
	refPC, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer refPC.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := refPC.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = refPC.WriteTo(buildTestDNSResponse(buf[:n], [4]byte{142, 250, 1, 1}), addr)
		}
	}()

	// DoH returns passthrough (matches reference!)
	dohSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q, _ := base64.RawURLEncoding.DecodeString(r.URL.Query().Get("dns"))
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(buildTestDNSResponse(q, [4]byte{142, 250, 1, 1}))
	}))
	defer dohSrv.Close()

	// Secondary UDP provider returns substituted IP 203.0.113.77
	subPC, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer subPC.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := subPC.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = subPC.WriteTo(buildTestDNSResponse(buf[:n], [4]byte{203, 0, 113, 77}), addr)
		}
	}()

	pool := NewResolverPool("127.0.0.1:0", dohSrv.URL)
	pool.DohFallbackIPs = nil
	pool.ReferenceAddrs = []string{refPC.LocalAddr().String()}
	pool.UDPProviders = []udpProvider{
		{name: "mock-sub", addrs: []string{subPC.LocalAddr().String()}},
	}

	ips, sub, err := pool.ResolveGateHost(context.Background(), "cloudcode-pa.googleapis.com")
	if err != nil {
		t.Fatalf("ResolveGateHost failed: %v", err)
	}
	if !sub || len(ips) != 1 || ips[0] != "203.0.113.77" {
		t.Fatalf("expected substituted 203.0.113.77, got ips=%v sub=%v", ips, sub)
	}
}

func TestDoHAndConnectTunnelEndToEnd(t *testing.T) {
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoLn.Close()

	go func() {
		for {
			c, err := echoLn.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}(c)
		}
	}()

	_, targetPort, _ := net.SplitHostPort(echoLn.Addr().String())

	var dohCalls int
	dohSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dohCalls++
		rawDNS := r.URL.Query().Get("dns")
		q, err := base64.RawURLEncoding.DecodeString(rawDNS)
		if err != nil || len(q) < 12 {
			http.Error(w, "bad dns query", http.StatusBadRequest)
			return
		}
		if !strings.Contains(string(q[12:]), "cloudcode-pa") {
			http.Error(w, "unexpected domain in query", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(buildTestDNSResponse(q, [4]byte{127, 0, 0, 1}))
	}))
	defer dohSrv.Close()

	proxySrv := NewServer("127.0.0.1:0", dohSrv.URL)
	proxySrv.Pool.DohFallbackIPs = nil
	proxySrv.Pool.UDPProviders = nil
	proxySrv.Pool.ReferenceAddrs = nil

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveDone := make(chan error, 1)
	go func() {
		serveDone <- proxySrv.Serve(ctx, proxyLn)
	}()

	conn, err := net.DialTimeout("tcp", proxyLn.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	req := "CONNECT cloudcode-pa.googleapis.com:" + targetPort + " HTTP/1.1\r\nHost: cloudcode-pa.googleapis.com:" + targetPort + "\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("ReadResponse failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}

	msg := "hello tunnel"
	if _, err := conn.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(br, buf); err != nil {
		t.Fatalf("failed reading echoed bytes: %v", err)
	}
	if string(buf) != msg {
		t.Fatalf("echo mismatch: got %q, want %q", string(buf), msg)
	}
	if dohCalls != 1 {
		t.Fatalf("expected 1 DoH call, got %d", dohCalls)
	}

	// Second resolution hits cache.
	ips, _, err := proxySrv.Pool.ResolveGateHost(context.Background(), "cloudcode-pa.googleapis.com")
	if err != nil || len(ips) == 0 || ips[0] != "127.0.0.1" {
		t.Fatalf("cached resolve failed: ips=%v err=%v", ips, err)
	}
	if dohCalls != 1 {
		t.Fatalf("expected cached DoH call count 1, got %d", dohCalls)
	}

	proxyURL, _ := url.Parse("http://" + proxyLn.Addr().String())
	httpClient := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	nonConnectResp, err := httpClient.Get("http://example.com/")
	if err != nil {
		t.Fatal(err)
	}
	nonConnectResp.Body.Close()
	if nonConnectResp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for non-CONNECT, got %d", nonConnectResp.StatusCode)
	}

	cancel()
	if err := <-serveDone; err != nil {
		t.Fatalf("proxy Serve returned error on shutdown: %v", err)
	}
}

func TestResolverPoolEarlyExitAndSingleflight(t *testing.T) {
	refPC, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer refPC.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := refPC.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = refPC.WriteTo(buildTestDNSResponse(buf[:n], [4]byte{142, 250, 1, 1}), addr)
		}
	}()

	// Silent UDP socket that never replies (simulates blackholed fallback UDP provider).
	silentPC, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer silentPC.Close()

	var dohHits atomic.Int32
	dohSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dohHits.Add(1)
		q, _ := base64.RawURLEncoding.DecodeString(r.URL.Query().Get("dns"))
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(buildTestDNSResponse(q, [4]byte{203, 0, 113, 99}))
	}))
	defer dohSrv.Close()

	pool := NewResolverPool("127.0.0.1:0", dohSrv.URL)
	pool.DohFallbackIPs = nil
	pool.ReferenceAddrs = []string{refPC.LocalAddr().String()}
	pool.UDPProviders = []udpProvider{
		{name: "blackholed", addrs: []string{silentPC.LocalAddr().String()}},
	}

	start := time.Now()
	const workers = 8
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			ips, sub, err := pool.ResolveGateHost(context.Background(), "cloudcode-pa.googleapis.com")
			if err != nil || !sub || len(ips) == 0 || ips[0] != "203.0.113.99" {
				t.Errorf("unexpected resolve result: ips=%v sub=%v err=%v", ips, sub, err)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	if dohHits.Load() != 1 {
		t.Fatalf("expected singleflight 1 DoH hit across %d concurrent callers, got %d", workers, dohHits.Load())
	}
	if elapsed >= 900*time.Millisecond {
		t.Fatalf("expected early exit before blackholed UDP timeout, took %v", elapsed)
	}
}

func TestResolverPoolMergesSecondaryDoHAndOrderFallbackIPs(t *testing.T) {
	refPC, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer refPC.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := refPC.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = refPC.WriteTo(buildTestDNSResponse(buf[:n], [4]byte{142, 250, 1, 1}), addr)
		}
	}()

	doh1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q, _ := base64.RawURLEncoding.DecodeString(r.URL.Query().Get("dns"))
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(buildTestDNSResponse(q, [4]byte{203, 0, 113, 10}, [4]byte{203, 0, 113, 20}))
	}))
	defer doh1.Close()

	doh2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q, _ := base64.RawURLEncoding.DecodeString(r.URL.Query().Get("dns"))
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(buildTestDNSResponse(q, [4]byte{203, 0, 113, 30}, [4]byte{203, 0, 113, 10}))
	}))
	defer doh2.Close()

	pool := NewResolverPool("127.0.0.1:0", doh1.URL)
	pool.DohFallbackIPs = []string{"198.51.100.1", "198.51.100.2"}
	pool.SecondaryDoHs = []dohProvider{{name: "backup-doh", url: doh2.URL}}
	pool.UDPProviders = nil
	pool.ReferenceAddrs = []string{refPC.LocalAddr().String()}

	// Preference order must stay deterministic until an IP is benched.
	gotOrder := pool.orderFallbackList(pool.DohFallbackIPs)
	if len(gotOrder) != 2 || gotOrder[0] != "198.51.100.1" {
		t.Fatalf("expected primary fallback first, got %v", gotOrder)
	}
	pool.benchFallbackIP("198.51.100.1", time.Minute)
	gotOrder = pool.orderFallbackList(pool.DohFallbackIPs)
	if len(gotOrder) != 2 || gotOrder[0] != "198.51.100.2" {
		t.Fatalf("expected benched fallback demoted, got %v", gotOrder)
	}

	ips, sub, err := pool.ResolveGateHost(context.Background(), "daily-cloudcode-pa.googleapis.com")
	if err != nil || !sub {
		t.Fatalf("ResolveGateHost failed: ips=%v sub=%v err=%v", ips, sub, err)
	}
	want := []string{"203.0.113.10", "203.0.113.20", "203.0.113.30"}
	if strings.Join(ips, ",") != strings.Join(want, ",") {
		t.Fatalf("expected merged substituted IPs %v, got %v", want, ips)
	}
}

func TestTLSHandshakeFailoverOnStalledRelay(t *testing.T) {
	refPC, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer refPC.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := refPC.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = refPC.WriteTo(buildTestDNSResponse(buf[:n], [4]byte{142, 250, 1, 1}), addr)
		}
	}()

	// Stalled relay: accepts TCP and silently consumes ClientHello without replying (simulates hung SNI relay).
	stalledLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer stalledLn.Close()
	var stalledHits atomic.Int32
	go func() {
		for {
			c, err := stalledLn.Accept()
			if err != nil {
				return
			}
			stalledHits.Add(1)
			go func(conn net.Conn) {
				defer conn.Close()
				buf := make([]byte, 2048)
				_, _ = conn.Read(buf)
				time.Sleep(2 * time.Second)
			}(c)
		}
	}()

	goodSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "relayed ok")
	}))
	defer goodSrv.Close()
	goodAddr := strings.TrimPrefix(goodSrv.URL, "https://")

	dohSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q, _ := base64.RawURLEncoding.DecodeString(r.URL.Query().Get("dns"))
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(buildTestDNSResponse(q, [4]byte{203, 0, 113, 1}, [4]byte{203, 0, 113, 2}))
	}))
	defer dohSrv.Close()

	proxySrv := NewServer("127.0.0.1:0", dohSrv.URL)
	proxySrv.Pool.DohFallbackIPs = nil
	proxySrv.Pool.UDPProviders = nil
	proxySrv.Pool.ReferenceAddrs = []string{refPC.LocalAddr().String()}
	proxySrv.tlsFirstByteTimeout = 150 * time.Millisecond
	proxySrv.testDialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		d := &net.Dialer{Timeout: time.Second}
		switch addr {
		case "203.0.113.1:443":
			return d.DialContext(ctx, network, stalledLn.Addr().String())
		case "203.0.113.2:443":
			return d.DialContext(ctx, network, goodAddr)
		default:
			return nil, fmt.Errorf("unexpected dial target %s", addr)
		}
	}

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- proxySrv.Serve(ctx, proxyLn) }()

	proxyURL, _ := url.Parse("http://" + proxyLn.Addr().String())
	newClient := func() *http.Client {
		return &http.Client{
			Timeout: 4 * time.Second,
			Transport: &http.Transport{
				Proxy:             http.ProxyURL(proxyURL),
				DisableKeepAlives: true,
				TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
			},
		}
	}

	resp, err := newClient().Get("https://daily-cloudcode-pa.googleapis.com/check")
	if err != nil {
		t.Fatalf("expected TLS failover to recover seamlessly, got err: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "relayed ok" {
		t.Fatalf("unexpected response body: %q", string(body))
	}
	if stalledHits.Load() != 1 {
		t.Fatalf("expected stalled relay tried once on first connect, got %d", stalledHits.Load())
	}

	// Subsequent connection should skip the benched stalled relay immediately.
	resp2, err := newClient().Get("https://daily-cloudcode-pa.googleapis.com/check")
	if err != nil {
		t.Fatalf("second request failed: %v", err)
	}
	resp2.Body.Close()
	if stalledHits.Load() != 1 {
		t.Fatalf("expected stalled relay to stay benched on second request, got hits=%d", stalledHits.Load())
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}
}

func TestStaggeredDialSkipsDeadFirstRelayQuickly(t *testing.T) {
	srv := NewServer("127.0.0.1:0", "http://127.0.0.1:0")
	srv.testDialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr == "203.0.113.1:443" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		c1, c2 := net.Pipe()
		_ = c2.Close()
		return c1, nil
	}

	start := time.Now()
	conn, target, rem, err := srv.dialBoundedIPs(context.Background(), []string{"203.0.113.1", "203.0.113.2"}, "443", 3*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("dialBoundedIPs failed: %v", err)
	}
	_ = conn.Close()
	if target != "203.0.113.2:443" {
		t.Fatalf("expected healthy second relay, got %s", target)
	}
	if len(rem) != 1 || rem[0] != "203.0.113.1" {
		t.Fatalf("unexpected remaining slice: %v", rem)
	}
	if elapsed >= 1200*time.Millisecond {
		t.Fatalf("staggered dial took too long: %v", elapsed)
	}
}

func TestLoopbackAddrFromURLAndBindCandidates(t *testing.T) {
	if got := LoopbackAddrFromURL("http://127.0.0.1:41234/"); got != "127.0.0.1:41234" {
		t.Fatalf("LoopbackAddrFromURL = %q, want 127.0.0.1:41234", got)
	}
	if got := LoopbackAddrFromURL("http://localhost:38451"); got != "127.0.0.1:38451" {
		t.Fatalf("LoopbackAddrFromURL localhost = %q", got)
	}
	if got := LoopbackAddrFromURL("http://192.168.1.5:8080"); got != "" {
		t.Fatalf("expected non-loopback rejected, got %q", got)
	}

	srv := NewServer(ListenAddr, "")
	srv.PreferredAddr = "http://127.0.0.1:39999"
	cands := srv.bindCandidates()
	if len(cands) < 3 || cands[0] != "127.0.0.1:39999" || cands[1] != ListenAddr || cands[len(cands)-1] != "127.0.0.1:0" {
		t.Fatalf("unexpected bindCandidates order: %v", cands)
	}
}

func TestParseAndFormatDNSProviderInput(t *testing.T) {
	doh, err := ParseDNSProviderInput("Quad9 | https://dns.quad9.net/dns-query | 9.9.9.9,149.112.112.112")
	if err != nil {
		t.Fatalf("ParseDNSProviderInput DoH failed: %v", err)
	}
	if doh.Name != "Quad9" || doh.Kind != DNSKindDoH || doh.Target != "https://dns.quad9.net/dns-query" || len(doh.FallbackIPs) != 2 {
		t.Fatalf("unexpected parsed DoH: %+v", doh)
	}
	if formatted := FormatDNSProviderInput(doh); formatted != "Quad9 | https://dns.quad9.net/dns-query | 9.9.9.9,149.112.112.112" {
		t.Fatalf("unexpected FormatDNSProviderInput: %q", formatted)
	}

	udp, err := ParseDNSProviderInput("83.220.169.155, 212.109.195.93:5353")
	if err != nil {
		t.Fatalf("ParseDNSProviderInput UDP failed: %v", err)
	}
	if udp.Kind != DNSKindUDP || udp.Target != "83.220.169.155:53" || len(udp.FallbackIPs) != 1 || udp.FallbackIPs[0] != "212.109.195.93:5353" {
		t.Fatalf("unexpected parsed UDP: %+v", udp)
	}

	if _, err := ParseDNSProviderInput("http://insecure.example/dns-query"); err == nil {
		t.Fatalf("expected non-HTTPS DoH URL rejected")
	}

	dot, err := ParseDNSProviderInput("Geodema | tls://dns.geodema.love | 1.2.3.4")
	if err != nil {
		t.Fatalf("ParseDNSProviderInput DoT failed: %v", err)
	}
	if dot.Kind != DNSKindDoT || dot.Name != "Geodema" || dot.Target != "tls://dns.geodema.love" || len(dot.FallbackIPs) != 1 {
		t.Fatalf("unexpected parsed DoT: %+v", dot)
	}

	bare, err := ParseDNSProviderInput("d.webshiftvpn.ru")
	if err != nil {
		t.Fatalf("ParseDNSProviderInput bare host failed: %v", err)
	}
	if bare.Kind != DNSKindDoT || bare.Target != "tls://d.webshiftvpn.ru" {
		t.Fatalf("unexpected bare hostname result: %+v", bare)
	}
}

func TestEmbeddedCatalogAndRepoSync(t *testing.T) {
	cat := EmbeddedCatalog()
	if len(cat.Providers) < 5 {
		t.Fatalf("expected embedded dns/providers.json to load multiple entries, got %d", len(cat.Providers))
	}
	hasDNSAI, hasEuGeo, hasComss := false, false, false
	for _, p := range cat.Providers {
		if p.Name == "dns-ai.ru" && p.Enabled {
			hasDNSAI = true
		}
		if p.Name == "eu.geohide.ru" && p.Enabled {
			hasEuGeo = true
		}
		if strings.Contains(strings.ToLower(p.Name), "comss") {
			hasComss = true
		}
	}
	if !hasDNSAI || !hasEuGeo || hasComss {
		t.Fatalf("unexpected embedded catalog defaults: dnsAI=%v euGeo=%v comss=%v", hasDNSAI, hasEuGeo, hasComss)
	}

	catSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"version": 1,
			"updated": "2026-11-12",
			"providers": [
				{"name": "dns-ai.ru", "kind": "doh", "target": "https://dns.dns-ai.ru/dns-query", "fallback_ips": ["194.87.196.13"], "enabled": true},
				{"name": "new.community", "kind": "dot", "target": "tls://new.community", "enabled": false}
			]
		}`))
	}))
	defer catSrv.Close()

	epSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"v":1,"serial":9,"v4":["198.51.100.77","198.51.100.88"]}`))
	}))
	defer epSrv.Close()

	fetched, err := FetchRepoCatalog(context.Background(), catSrv.URL, epSrv.URL)
	if err != nil {
		t.Fatalf("FetchRepoCatalog failed: %v", err)
	}
	if len(fetched.Providers) != 2 || len(fetched.Providers[0].FallbackIPs) != 2 || fetched.Providers[0].FallbackIPs[0] != "198.51.100.77" {
		t.Fatalf("expected live DNS-AI IPs merged into fetched catalog, got %+v", fetched.Providers)
	}

	existing := []DNSProviderSpec{
		{Name: "dns-ai.ru", Kind: DNSKindDoH, Target: DohURL, FallbackIPs: []string{"1.2.3.4"}, Enabled: false, Source: DNSSourceCatalog},
		{Name: "retired.cat", Kind: DNSKindUDP, Target: "198.51.100.9:53", Enabled: true, Source: DNSSourceCatalog},
		{Name: "MyUserDNS", Kind: DNSKindDoH, Target: "https://my.user.dns/dns-query", Enabled: true, Source: DNSSourceUser},
	}
	synced, stats := SyncCatalogProviders(existing, nil, fetched.Providers)
	if stats.Added != 1 || stats.Updated != 1 || stats.Removed != 1 || len(synced) != 3 {
		t.Fatalf("unexpected sync stats %+v synced=%+v", stats, synced)
	}
	if synced[0].Enabled {
		t.Fatalf("expected user's disabled preference preserved during catalog sync")
	}
	if synced[1].Target != "https://my.user.dns/dns-query" || synced[1].Source != DNSSourceUser {
		t.Fatalf("expected user-owned DNS preserved untouched, got %+v", synced[1])
	}

	// Verify 1-hop feed_url delegation + plain-text external feed support.
	txtSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("# comment line\nhttps://ext.doh.example/dns-query\nExt DoT | tls://dot.ext.example\n"))
	}))
	defer txtSrv.Close()
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(fmt.Sprintf(`{"version":1,"updated":"2026-11-12","feed_url":%q}`, txtSrv.URL)))
	}))
	defer hubSrv.Close()

	delegated, err := FetchRepoCatalog(context.Background(), hubSrv.URL, "")
	if err != nil {
		t.Fatalf("delegated FetchRepoCatalog failed: %v", err)
	}
	if len(delegated.Providers) != 2 || delegated.Providers[1].Kind != DNSKindDoT || delegated.Providers[0].Enabled {
		t.Fatalf("unexpected delegated plain-text catalog: %+v", delegated.Providers)
	}
}

func TestProbeDNSProvidersClassification(t *testing.T) {
	refPC, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer refPC.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := refPC.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = refPC.WriteTo(buildTestDNSResponse(buf[:n], [4]byte{142, 250, 1, 1}), addr)
		}
	}()

	subSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q, _ := base64.RawURLEncoding.DecodeString(r.URL.Query().Get("dns"))
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(buildTestDNSResponse(q, [4]byte{203, 0, 113, 55}))
	}))
	defer subSrv.Close()

	tlsFailSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q, _ := base64.RawURLEncoding.DecodeString(r.URL.Query().Get("dns"))
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(buildTestDNSResponse(q, [4]byte{203, 0, 113, 99}))
	}))
	defer tlsFailSrv.Close()

	directSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q, _ := base64.RawURLEncoding.DecodeString(r.URL.Query().Get("dns"))
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(buildTestDNSResponse(q, [4]byte{142, 250, 1, 1}))
	}))
	defer directSrv.Close()

	list := []DNSProviderSpec{
		{Name: "sub-ok", Kind: DNSKindDoH, Target: subSrv.URL, Enabled: true},
		{Name: "sub-bad-tls", Kind: DNSKindDoH, Target: tlsFailSrv.URL, Enabled: false},
		{Name: "direct-only", Kind: DNSKindDoH, Target: directSrv.URL, Enabled: false},
		{Name: "dead-udp", Kind: DNSKindUDP, Target: "127.0.0.1:1", Enabled: false},
	}

	results := ProbeProviders(context.Background(), list, ProbeOptions{
		Hosts:          []string{"daily-cloudcode-pa.googleapis.com"},
		ReferenceAddrs: []string{refPC.LocalAddr().String()},
		Timeout:        800 * time.Millisecond,
		DialTLSCheck: func(ctx context.Context, ip, serverName string) error {
			if ip == "203.0.113.55" {
				return nil
			}
			return io.EOF
		},
	})
	if len(results) != 4 {
		t.Fatalf("expected 4 results, got %d", len(results))
	}
	if results[0].Status != ProbeSub || results[1].Status != ProbeTLSErr || results[2].Status != ProbeDirect || results[3].Status != ProbeErr {
		t.Fatalf("unexpected probe classification: %+v", results)
	}
}

func TestResolverPoolStrictAndReorderedCustomDNS(t *testing.T) {
	refPC, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer refPC.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := refPC.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = refPC.WriteTo(buildTestDNSResponse(buf[:n], [4]byte{142, 250, 1, 1}), addr)
		}
	}()

	var hitsA, hitsB atomic.Int32
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsA.Add(1)
		q, _ := base64.RawURLEncoding.DecodeString(r.URL.Query().Get("dns"))
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(buildTestDNSResponse(q, [4]byte{203, 0, 113, 10}))
	}))
	defer srvA.Close()

	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsB.Add(1)
		q, _ := base64.RawURLEncoding.DecodeString(r.URL.Query().Get("dns"))
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(buildTestDNSResponse(q, [4]byte{203, 0, 113, 20}))
	}))
	defer srvB.Close()

	pool := NewResolverPool("127.0.0.1:0", srvA.URL)
	pool.ReferenceAddrs = []string{refPC.LocalAddr().String()}

	// Strict mode queries only priority #1 when it responds cleanly.
	pool.UpdateDNS(DNSModeStrict, []DNSProviderSpec{
		{Name: "Alpha", Kind: DNSKindDoH, Target: srvA.URL, Enabled: true},
		{Name: "Beta", Kind: DNSKindDoH, Target: srvB.URL, Enabled: true},
	})
	ips, sub, err := pool.ResolveGateHost(context.Background(), "cloudcode-pa.googleapis.com")
	if err != nil || !sub || len(ips) != 1 || ips[0] != "203.0.113.10" {
		t.Fatalf("strict mode #1 mismatch: ips=%v sub=%v err=%v", ips, sub, err)
	}
	if hitsA.Load() != 1 || hitsB.Load() != 0 {
		t.Fatalf("expected strict mode not to call #2 when #1 succeeded (hitsA=%d hitsB=%d)", hitsA.Load(), hitsB.Load())
	}

	// Switching order to Beta first + disabling Alpha takes effect immediately via UpdateDNS.
	pool.UpdateDNS(DNSModeAuto, []DNSProviderSpec{
		{Name: "Beta", Kind: DNSKindDoH, Target: srvB.URL, Enabled: true},
		{Name: "Alpha", Kind: DNSKindDoH, Target: srvA.URL, Enabled: false},
	})
	ips2, sub2, err := pool.ResolveGateHost(context.Background(), "cloudcode-pa.googleapis.com")
	if err != nil || !sub2 || len(ips2) != 1 || ips2[0] != "203.0.113.20" {
		t.Fatalf("updated DNS order mismatch: ips=%v sub=%v err=%v", ips2, sub2, err)
	}
	if hitsA.Load() != 1 || hitsB.Load() != 1 {
		t.Fatalf("expected disabled Alpha skipped and Beta queried once (hitsA=%d hitsB=%d)", hitsA.Load(), hitsB.Load())
	}
}
