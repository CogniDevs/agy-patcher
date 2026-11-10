package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ListenAddr is the primary loopback endpoint used by agy-patcher's embedded proxy.
// Chosen outside Windows Hyper-V/WSL2 dynamic port ranges (49152-65535); falls back cleanly to BackupAddrs and ephemeral 127.0.0.1:0.
const ListenAddr = "127.0.0.1:38451"

// FallbackAddrs are tried in order when ListenAddr is occupied, followed by an OS-assigned ephemeral loopback port.
var FallbackAddrs = []string{
	"127.0.0.1:38452",
	"127.0.0.1:38453",
	"127.0.0.1:38454",
}

var GateHosts = map[string]bool{
	"cloudcode-pa.googleapis.com":       true,
	"daily-cloudcode-pa.googleapis.com": true,
	"aicode.googleapis.com":             true,
	"generativelanguage.googleapis.com": true,
}

var copyBufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 32*1024)
		return &buf
	},
}

// Stats holds live proxy connection counters.
type Stats struct {
	Active int64
	Total  int64
	Gate   int64
}

// Server implements a local HTTP CONNECT proxy that routes GateHosts through split-DNS/DoH relay pools.
type Server struct {
	ListenAddr    string
	PreferredAddr string // optional loopback addr (e.g. previous AGY_PROXY_U) tried before ListenAddr
	Pool          *ResolverPool
	Dialer        *net.Dialer
	OnConnect     func(host string, gate bool, target string, err error)

	activeConns atomic.Int64
	totalConns  atomic.Int64
	gateConns   atomic.Int64
	initOnce    sync.Once

	ipMu        sync.Mutex
	gateBenched map[string]time.Time
	gateFails   map[string]int

	testDialFunc        func(ctx context.Context, network, addr string) (net.Conn, error)
	tlsFirstByteTimeout time.Duration
}

// Stats returns a snapshot of active, total, and gate connection counts.
func (s *Server) Stats() Stats {
	return Stats{
		Active: s.activeConns.Load(),
		Total:  s.totalConns.Load(),
		Gate:   s.gateConns.Load(),
	}
}

// NewServer creates a proxy Server with default DoH + UDP split-DNS pool.
func NewServer(listenAddr, dohURL string) *Server {
	if listenAddr == "" {
		listenAddr = ListenAddr
	}
	return &Server{
		ListenAddr: listenAddr,
		Pool:       NewResolverPool(listenAddr, dohURL),
		Dialer:     &net.Dialer{Timeout: 10 * time.Second},
	}
}

// StartProxy listens on ListenAddr (falling back through FallbackAddrs if needed),
// calls onReady with the bound address, and blocks until ctx is cancelled.
func StartProxy(ctx context.Context, onReady func(addr string)) error {
	return NewServer(ListenAddr, DohURL).Run(ctx, onReady)
}

// Run binds PreferredAddr / ListenAddr (falling back through FallbackAddrs and finally 127.0.0.1:0 when using default ListenAddr),
// invokes onReady with the bound address, and serves CONNECT requests until ctx is cancelled.
func (s *Server) Run(ctx context.Context, onReady func(addr string)) error {
	candidates := s.bindCandidates()

	var listener net.Listener
	var bindErr error
	for _, addr := range candidates {
		listener, bindErr = net.Listen("tcp", addr)
		if bindErr == nil {
			break
		}
	}
	if listener == nil {
		return fmt.Errorf("failed to bind proxy listener (%s): %w", s.ListenAddr, bindErr)
	}

	boundAddr := listener.Addr().String()
	s.pool().RecordSelfAddr(boundAddr)
	if onReady != nil {
		onReady(boundAddr)
	}

	return s.Serve(ctx, listener)
}

func (s *Server) bindCandidates() []string {
	listenAddr := s.ListenAddr
	if listenAddr == "" {
		listenAddr = ListenAddr
	}
	if listenAddr != ListenAddr {
		return []string{listenAddr}
	}

	var raw []string
	if pref := LoopbackAddrFromURL(s.PreferredAddr); pref != "" {
		raw = append(raw, pref)
	}
	raw = append(raw, listenAddr)
	raw = append(raw, FallbackAddrs...)
	raw = append(raw, "127.0.0.1:0")

	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, a := range raw {
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	return out
}

// LoopbackAddrFromURL extracts a normalized "127.0.0.1:PORT" loopback socket address from a URL or host:port string.
func LoopbackAddrFromURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if !strings.Contains(trimmed, "://") {
		trimmed = "http://" + trimmed
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if host != "127.0.0.1" && host != "localhost" {
		return ""
	}
	portStr := u.Port()
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return ""
	}
	return net.JoinHostPort("127.0.0.1", portStr)
}

// Serve runs the proxy on an already-bound listener until ctx is cancelled.
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	httpSrv := &http.Server{
		Handler:           http.HandlerFunc(s.handleConnect),
		ReadHeaderTimeout: 120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- httpSrv.Serve(listener)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
		_ = httpSrv.Close()
		err := <-errCh
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// IsProxyRunning probes whether a TCP service is listening on addr.
func IsProxyRunning(addr string) bool {
	if addr == "" {
		addr = ListenAddr
	}
	conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	hostname, port, err := splitHostPortDefault(r.Host, "443")
	if err != nil || hostname == "" {
		http.Error(w, "Invalid host", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	isGate := GateHosts[strings.ToLower(hostname)]
	s.totalConns.Add(1)
	if isGate {
		s.gateConns.Add(1)
	}

	var destConn net.Conn
	var dialedTarget string
	var remainingIPs []string
	var subResolved bool
	if isGate {
		pool := s.pool()
		var ips []string
		var resolveErr error
		ips, subResolved, resolveErr = pool.ResolveGateHost(ctx, hostname)
		if resolveErr != nil {
			if s.OnConnect != nil {
				s.OnConnect(hostname, true, "", resolveErr)
			}
			http.Error(w, fmt.Sprintf("DNS resolution failed: %v", resolveErr), http.StatusBadGateway)
			return
		}
		destConn, dialedTarget, remainingIPs, err = s.dialBoundedIPs(ctx, ips, port, 4*time.Second)
		if err != nil {
			pool.Invalidate(hostname)
		}
	} else {
		dialedTarget = net.JoinHostPort(hostname, port)
		destConn, err = s.dialTCP(ctx, dialedTarget)
	}

	if err != nil {
		if s.OnConnect != nil {
			s.OnConnect(hostname, isGate, dialedTarget, err)
		}
		http.Error(w, fmt.Sprintf("Connection failed: %v", err), http.StatusBadGateway)
		return
	}
	if s.OnConnect != nil && isGate {
		s.OnConnect(hostname, true, dialedTarget, nil)
	}

	s.activeConns.Add(1)
	defer s.activeConns.Add(-1)

	hj, ok := w.(http.Hijacker)
	if !ok {
		destConn.Close()
		http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
		return
	}

	clientConn, brw, err := hj.Hijack()
	if err != nil {
		destConn.Close()
		http.Error(w, fmt.Sprintf("Hijack failed: %v", err), http.StatusServiceUnavailable)
		return
	}
	defer clientConn.Close()

	_ = clientConn.SetDeadline(time.Time{})

	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		destConn.Close()
		return
	}

	allowTLSFailover := isGate && subResolved && port == "443" && len(remainingIPs) > 0
	s.bridgeTunnel(clientConn, brw, destConn, allowTLSFailover, hostname, dialedTarget, port, remainingIPs)
}

func (s *Server) bridgeTunnel(
	clientConn net.Conn,
	brw *bufio.ReadWriter,
	destConn net.Conn,
	allowTLSFailover bool,
	hostname, dialedTarget, port string,
	remainingIPs []string,
) {
	initial, isHello := readInitialClientPayload(clientConn, brw, allowTLSFailover)
	if allowTLSFailover && isHello {
		liveDest, ok := s.dialWithTLSFailover(clientConn, destConn, initial, hostname, dialedTarget, port, remainingIPs)
		if !ok {
			return
		}
		destConn = liveDest
	} else if len(initial) > 0 {
		if _, err := destConn.Write(initial); err != nil {
			destConn.Close()
			return
		}
	}
	defer destConn.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		copyPooled(destConn, clientConn)
		closeWrite(destConn)
	}()
	go func() {
		defer wg.Done()
		copyPooled(clientConn, destConn)
		closeWrite(clientConn)
	}()
	wg.Wait()
}

const maxBufferedHello = 16 * 1024

func readInitialClientPayload(clientConn net.Conn, brw *bufio.ReadWriter, inspectHello bool) ([]byte, bool) {
	var buf []byte
	if brw != nil && brw.Reader != nil {
		if n := brw.Reader.Buffered(); n > 0 {
			chunk := make([]byte, n)
			if readN, err := io.ReadFull(brw.Reader, chunk); readN > 0 {
				buf = append(buf, chunk[:readN]...)
			} else if err != nil && len(buf) == 0 {
				return nil, false
			}
		}
	}
	if !inspectHello {
		return buf, false
	}

	tmp := make([]byte, 4096)
	if len(buf) == 0 {
		_ = clientConn.SetReadDeadline(time.Now().Add(800 * time.Millisecond))
		n, err := clientConn.Read(tmp)
		_ = clientConn.SetReadDeadline(time.Time{})
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil && len(buf) == 0 {
			return nil, false
		}
	}

	if len(buf) == 0 || buf[0] != 0x16 {
		return buf, false
	}

	// Ensure full TLS ClientHello record if fragmented across localhost reads.
	recDeadline := time.Now().Add(250 * time.Millisecond)
	_ = clientConn.SetReadDeadline(recDeadline)
	for len(buf) < 5 && len(buf) < maxBufferedHello {
		n, err := clientConn.Read(tmp[:5-len(buf)])
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			break
		}
	}
	if len(buf) >= 5 && buf[1] == 0x03 {
		want := 5 + (int(buf[3])<<8 | int(buf[4]))
		if want <= maxBufferedHello {
			for len(buf) < want {
				need := want - len(buf)
				if need > len(tmp) {
					need = len(tmp)
				}
				n, err := clientConn.Read(tmp[:need])
				if n > 0 {
					buf = append(buf, tmp[:n]...)
				}
				if err != nil {
					break
				}
			}
		}
	}
	_ = clientConn.SetReadDeadline(time.Time{})

	return buf, len(buf) >= 5 && buf[0] == 0x16 && buf[1] == 0x03
}

func (s *Server) dialWithTLSFailover(
	clientConn net.Conn,
	destConn net.Conn,
	helloBuf []byte,
	hostname, dialedTarget, port string,
	remainingIPs []string,
) (net.Conn, bool) {
	initialIP, _, _ := net.SplitHostPort(dialedTarget)
	budgetDeadline := time.Now().Add(6500 * time.Millisecond)

	currConn := destConn
	currIP := initialIP
	currTarget := dialedTarget
	nextIdx := 0

	respBuf := make([]byte, 4096)
	for {
		var attemptErr error
		_ = currConn.SetWriteDeadline(time.Now().Add(1500 * time.Millisecond))
		if _, err := currConn.Write(helloBuf); err != nil {
			attemptErr = err
		} else {
			_ = currConn.SetWriteDeadline(time.Time{})
			wait := s.tlsHandshakeTimeout()
			if left := time.Until(budgetDeadline); left < wait {
				wait = left
			}
			if wait <= 0 {
				attemptErr = context.DeadlineExceeded
			} else {
				_ = currConn.SetReadDeadline(time.Now().Add(wait))
				n, err := currConn.Read(respBuf)
				_ = currConn.SetReadDeadline(time.Time{})
				if n > 0 {
					if currIP != "" {
						s.recordGateIPOK(currIP)
					}
					if _, wErr := clientConn.Write(respBuf[:n]); wErr != nil {
						currConn.Close()
						return nil, false
					}
					if currTarget != dialedTarget && s.OnConnect != nil {
						s.OnConnect(hostname, true, currTarget, nil)
					}
					return currConn, true
				}
				if err == nil {
					err = io.ErrUnexpectedEOF
				}
				attemptErr = err
			}
		}

		currConn.Close()
		if currIP != "" {
			s.benchGateIP(currIP)
		}
		if s.OnConnect != nil {
			s.OnConnect(hostname, true, currTarget, fmt.Errorf("tls handshake failed: %w", attemptErr))
		}

		// Dial next healthy candidate relay IP within remaining budget.
		var dialedNext net.Conn
		var dialedIP, dialedAddr string
		for nextIdx < len(remainingIPs) {
			left := time.Until(budgetDeadline)
			if left <= 150*time.Millisecond {
				break
			}
			candIP := remainingIPs[nextIdx]
			nextIdx++
			candAddr := net.JoinHostPort(candIP, port)
			dialTimeout := 2 * time.Second
			if left < dialTimeout {
				dialTimeout = left
			}
			dCtx, dCancel := context.WithTimeout(context.Background(), dialTimeout)
			c, err := s.dialTCP(dCtx, candAddr)
			dCancel()
			if err != nil {
				s.benchGateIP(candIP)
				continue
			}
			dialedNext = c
			dialedIP = candIP
			dialedAddr = candAddr
			break
		}
		if dialedNext == nil {
			return nil, false
		}
		currConn = dialedNext
		currIP = dialedIP
		currTarget = dialedAddr
	}
}

func copyPooled(dst io.Writer, src io.Reader) {
	bufPtr := copyBufPool.Get().(*[]byte)
	defer copyBufPool.Put(bufPtr)
	_, _ = io.CopyBuffer(dst, src, *bufPtr)
}

func (s *Server) dialTCP(ctx context.Context, addr string) (net.Conn, error) {
	if s.testDialFunc != nil {
		return s.testDialFunc(ctx, "tcp", addr)
	}
	return s.dialer().DialContext(ctx, "tcp", addr)
}

func (s *Server) tlsHandshakeTimeout() time.Duration {
	if s.tlsFirstByteTimeout > 0 {
		return s.tlsFirstByteTimeout
	}
	return 2500 * time.Millisecond
}

func (s *Server) orderGateIPs(ips []string) []string {
	// IPv4 first (relay proxies are IPv4), keeping healthy non-benched candidates ahead of benched ones.
	now := time.Now()
	s.ipMu.Lock()
	var healthyV4, benchedV4, healthyV6, benchedV6 []string
	for _, raw := range ips {
		benched := false
		if until, ok := s.gateBenched[raw]; ok && now.Before(until) {
			benched = true
		}
		isV6 := false
		if parsed := net.ParseIP(raw); parsed != nil && parsed.To4() == nil {
			isV6 = true
		}
		switch {
		case !isV6 && !benched:
			healthyV4 = append(healthyV4, raw)
		case isV6 && !benched:
			healthyV6 = append(healthyV6, raw)
		case !isV6 && benched:
			benchedV4 = append(benchedV4, raw)
		default:
			benchedV6 = append(benchedV6, raw)
		}
	}
	s.ipMu.Unlock()

	ordered := make([]string, 0, len(ips))
	ordered = append(ordered, healthyV4...)
	ordered = append(ordered, healthyV6...)
	ordered = append(ordered, benchedV4...)
	ordered = append(ordered, benchedV6...)
	return ordered
}

func (s *Server) benchGateIP(ip string) {
	s.ipMu.Lock()
	defer s.ipMu.Unlock()
	if s.gateBenched == nil {
		s.gateBenched = make(map[string]time.Time)
	}
	if s.gateFails == nil {
		s.gateFails = make(map[string]int)
	}
	s.gateFails[ip]++
	fails := s.gateFails[ip]
	dur := 90 * time.Second
	for i := 1; i < fails && dur < 5*time.Minute; i++ {
		dur *= 2
	}
	if dur > 5*time.Minute {
		dur = 5 * time.Minute
	}
	s.gateBenched[ip] = time.Now().Add(dur)
}

func (s *Server) recordGateIPOK(ip string) {
	s.ipMu.Lock()
	delete(s.gateFails, ip)
	delete(s.gateBenched, ip)
	s.ipMu.Unlock()
}

func (s *Server) dialBoundedIPs(ctx context.Context, ips []string, port string, budget time.Duration) (net.Conn, string, []string, error) {
	if len(ips) == 0 {
		return nil, "", nil, fmt.Errorf("no IP addresses to dial")
	}

	ordered := s.orderGateIPs(ips)
	if len(ordered) == 1 {
		target := net.JoinHostPort(ordered[0], port)
		attemptCtx, cancel := context.WithTimeout(ctx, budget)
		conn, err := s.dialTCP(attemptCtx, target)
		cancel()
		if err != nil {
			s.benchGateIP(ordered[0])
			return nil, "", nil, err
		}
		return conn, target, nil, nil
	}

	// Happy-Eyeballs staggered dial across candidates so a stalled first relay SYN
	// never wastes a multi-second sequential timeout before trying healthy relays.
	dialCtx, cancelAll := context.WithTimeout(ctx, budget)
	defer cancelAll()

	type dialAttempt struct {
		idx    int
		ip     string
		target string
		conn   net.Conn
		err    error
	}

	results := make(chan dialAttempt, len(ordered))
	failed := make([]bool, len(ordered))
	completed := 0

	startAttempt := func(idx int) {
		ip := ordered[idx]
		target := net.JoinHostPort(ip, port)
		go func() {
			perTimeout := 2500 * time.Millisecond
			if dl, ok := dialCtx.Deadline(); ok {
				if left := time.Until(dl); left < perTimeout {
					perTimeout = left
				}
			}
			cCtx, cCancel := context.WithTimeout(dialCtx, perTimeout)
			conn, err := s.dialTCP(cCtx, target)
			cCancel()
			results <- dialAttempt{idx: idx, ip: ip, target: target, conn: conn, err: err}
		}()
	}

	const stagger = 350 * time.Millisecond
	started := 1
	startAttempt(0)

	var staggerTimer *time.Timer
	var staggerCh <-chan time.Time
	if started < len(ordered) {
		staggerTimer = time.NewTimer(stagger)
		staggerCh = staggerTimer.C
	}
	defer func() {
		if staggerTimer != nil {
			staggerTimer.Stop()
		}
	}()

	var lastErr error
	for completed < len(ordered) {
		select {
		case res := <-results:
			completed++
			if res.err == nil && res.conn != nil {
				cancelAll()
				// Drain already-returned concurrent wins cleanly without blocking on canceled dials.
				go func(rem int) {
					for i := 0; i < rem; i++ {
						extra := <-results
						if extra.conn != nil {
							_ = extra.conn.Close()
						}
					}
				}(started - completed)

				remaining := make([]string, 0, len(ordered)-1)
				for i, ip := range ordered {
					if i != res.idx && !failed[i] {
						remaining = append(remaining, ip)
					}
				}
				return res.conn, res.target, remaining, nil
			}
			failed[res.idx] = true
			if dialCtx.Err() == nil {
				s.benchGateIP(res.ip)
			}
			lastErr = res.err
			// Immediately launch next unstaggered candidate if current attempt errored fast.
			if started < len(ordered) {
				if staggerTimer != nil {
					staggerTimer.Stop()
				}
				startAttempt(started)
				started++
				if started < len(ordered) {
					staggerTimer = time.NewTimer(stagger)
					staggerCh = staggerTimer.C
				} else {
					staggerCh = nil
				}
			}
		case <-staggerCh:
			if started < len(ordered) {
				startAttempt(started)
				started++
			}
			if started < len(ordered) {
				staggerTimer.Reset(stagger)
			} else {
				staggerCh = nil
			}
		case <-dialCtx.Done():
			completed = len(ordered)
		}
	}

	if lastErr == nil {
		lastErr = context.DeadlineExceeded
	}
	return nil, "", nil, lastErr
}

func (s *Server) ensureDefaults() {
	s.initOnce.Do(func() {
		if s.Pool == nil {
			s.Pool = NewResolverPool(s.ListenAddr, DohURL)
		}
		if s.Dialer == nil {
			s.Dialer = &net.Dialer{Timeout: 10 * time.Second}
		}
	})
}

func (s *Server) dialer() *net.Dialer {
	s.ensureDefaults()
	return s.Dialer
}

func (s *Server) pool() *ResolverPool {
	s.ensureDefaults()
	return s.Pool
}

func splitHostPortDefault(hostport, defaultPort string) (string, string, error) {
	if hostport == "" {
		return "", "", fmt.Errorf("empty host")
	}
	if strings.Contains(hostport, ":") {
		if strings.HasPrefix(hostport, "[") && strings.HasSuffix(hostport, "]") {
			return strings.TrimSuffix(strings.TrimPrefix(hostport, "["), "]"), defaultPort, nil
		}
		host, port, err := net.SplitHostPort(hostport)
		if err == nil {
			if port == "" {
				port = defaultPort
			}
			return host, port, nil
		}
		if ip := net.ParseIP(hostport); ip != nil {
			return hostport, defaultPort, nil
		}
		return "", "", err
	}
	return hostport, defaultPort, nil
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}
