package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"agy-patcher/internal/hostenv"
	"agy-patcher/pkg/autostart"
	"agy-patcher/pkg/config"
	"agy-patcher/pkg/env"
	"agy-patcher/pkg/i18n"
	"agy-patcher/pkg/proxy"
)

const maxLogEntries = 1500

var (
	ErrAlreadyRunning = errors.New("daemon is already running")
	ErrNotRunning     = errors.New("daemon is not running")
)

// Info describes the active background daemon IPC endpoint, optional bound proxy address, and auth token.
type Info struct {
	PID       int    `json:"pid"`
	Addr      string `json:"addr"`
	ProxyAddr string `json:"proxy_addr,omitempty"`
	Token     string `json:"token"`
	Exe       string `json:"exe"`
	Version   string `json:"version"`
	Lang      string `json:"lang"`
}

// ProxySessionInfo records an active foreground CLI proxy instance so status and cleanup recognize dynamic ports.
type ProxySessionInfo struct {
	PID  int    `json:"pid"`
	Addr string `json:"addr"`
	Exe  string `json:"exe,omitempty"`
}

// LogEntry represents a timestamped event emitted by the daemon or its embedded proxy.
type LogEntry struct {
	Seq   uint64 `json:"seq"`
	TS    string `json:"ts"`
	Level string `json:"level"`
	Msg   string `json:"msg"`
}

// StateSnapshot captures a live view of daemon/proxy/autostart state plus new logs since `since`.
type StateSnapshot struct {
	PID           int                     `json:"pid"`
	Version       string                  `json:"version"`
	Exe           string                  `json:"exe"`
	Lang          string                  `json:"lang"`
	ProxyOwned    bool                    `json:"proxy_owned"`
	ProxyAddr     string                  `json:"proxy_addr"`
	ExternalProxy string                  `json:"external_proxy"`
	ProxyNote     string                  `json:"proxy_note"`
	Stats         proxy.Stats             `json:"stats"`
	DNSMode       string                  `json:"dns_mode,omitempty"`
	DNSProviders  []proxy.DNSProviderSpec `json:"dns_providers,omitempty"`
	AutostartOn   bool                    `json:"autostart_on"`
	Logs          []LogEntry              `json:"logs,omitempty"`
	NextSeq       uint64                  `json:"next_seq"`
}

// ServiceOptions configures a Daemon HTTP/proxy service instance.
type ServiceOptions struct {
	Version        string
	Lang           string
	ExePath        string
	ListenAddr     string // optional override (e.g. "127.0.0.1:0" in tests); defaults to proxy.ListenAddr
	SkipEnvPersist bool   // skip modifying user proxy env var (for unit tests)
	OnChange       func()
	CancelRoot     context.CancelFunc
}

// Service manages the single background proxy instance, log ring buffer, and authenticated loopback HTTP API.
type Service struct {
	mu            sync.Mutex
	version       string
	exePath       string
	listenAddr    string
	skipEnv       bool
	lang          string
	token         string
	info          Info
	statePath     string
	ln            net.Listener
	httpSrv       *http.Server
	cancelRoot    context.CancelFunc
	onChange      func()
	proxyServer   *proxy.Server
	proxyCancel   context.CancelFunc
	proxyDone     chan error
	proxyOwned    bool
	proxyAddr     string
	proxyNote     string
	externalProxy string
	autostartOn   bool
	dnsMode       string
	dnsProviders  []proxy.DNSProviderSpec
	logs          []LogEntry
	nextSeq       uint64
	shutting      bool
}

// EffectiveDNS resolves the active DNS mode and provider list from persisted config,
// falling back to proxy defaults unless the user explicitly customized the list, while
// ensuring separated UserDNSProviders remain intact and quarantined HiddenDNSKeys stay hidden.
func EffectiveDNS(cfg config.Config) (string, []proxy.DNSProviderSpec) {
	mode := proxy.NormalizeDNSMode(cfg.DNSMode)
	hidden := make(map[string]bool, len(cfg.HiddenDNSKeys))
	for _, k := range cfg.HiddenDNSKeys {
		if trimmed := strings.ToLower(strings.TrimSpace(k)); trimmed != "" {
			hidden[trimmed] = true
		}
	}

	userByKey := make(map[string]bool, len(cfg.UserDNSProviders))
	var userSpecs []proxy.DNSProviderSpec
	for _, u := range cfg.UserDNSProviders {
		sp := proxy.DNSProviderSpec{
			Name:        u.Name,
			Kind:        u.Kind,
			Target:      u.Target,
			FallbackIPs: append([]string(nil), u.FallbackIPs...),
			Enabled:     u.Enabled,
			Source:      proxy.DNSSourceUser,
		}
		key := proxy.ProviderSyncKey(sp)
		if key == "|" {
			continue
		}
		userByKey[key] = true
		userSpecs = append(userSpecs, sp)
	}

	var base []proxy.DNSProviderSpec
	if !cfg.DNSCustomized && len(cfg.DNSProviders) == 0 && len(userSpecs) == 0 {
		base = proxy.DefaultDNSProviders()
	} else {
		base = make([]proxy.DNSProviderSpec, 0, len(cfg.DNSProviders)+len(userSpecs))
		for _, p := range cfg.DNSProviders {
			sp := proxy.DNSProviderSpec{
				Name:        p.Name,
				Kind:        p.Kind,
				Target:      p.Target,
				FallbackIPs: append([]string(nil), p.FallbackIPs...),
				Enabled:     p.Enabled,
				Source:      strings.ToLower(strings.TrimSpace(p.Source)),
			}
			if userByKey[proxy.ProviderSyncKey(sp)] {
				sp.Source = proxy.DNSSourceUser
			}
			base = append(base, sp)
		}
	}

	out := make([]proxy.DNSProviderSpec, 0, len(base)+len(userSpecs))
	seen := make(map[string]bool, len(base)+len(userSpecs))
	for _, sp := range base {
		key := proxy.ProviderSyncKey(sp)
		if seen[key] {
			continue
		}
		if sp.Source == proxy.DNSSourceCatalog && hidden[key] {
			seen[key] = true
			continue
		}
		seen[key] = true
		out = append(out, sp)
	}
	for _, u := range userSpecs {
		key := proxy.ProviderSyncKey(u)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, u)
	}
	return proxy.NormalizeDNSMode(mode), out
}

// ConfigFromDNS converts runtime proxy DNS specs into persisted config fields.
func ConfigFromDNS(mode string, list []proxy.DNSProviderSpec) (string, []config.DNSProvider) {
	out := make([]config.DNSProvider, 0, len(list))
	for _, p := range list {
		out = append(out, config.DNSProvider{
			Name:        p.Name,
			Kind:        p.Kind,
			Target:      p.Target,
			FallbackIPs: append([]string(nil), p.FallbackIPs...),
			Enabled:     p.Enabled,
			Source:      p.Source,
		})
	}
	return proxy.NormalizeDNSMode(mode), out
}

// ExtractUserDNSProviders returns only user-owned DNS entries for separate persistence.
func ExtractUserDNSProviders(list []proxy.DNSProviderSpec) []config.DNSProvider {
	var out []config.DNSProvider
	for _, p := range list {
		if p.Source != proxy.DNSSourceUser {
			continue
		}
		out = append(out, config.DNSProvider{
			Name:        p.Name,
			Kind:        p.Kind,
			Target:      p.Target,
			FallbackIPs: append([]string(nil), p.FallbackIPs...),
			Enabled:     p.Enabled,
			Source:      proxy.DNSSourceUser,
		})
	}
	return out
}

func cloneDNSProviders(list []proxy.DNSProviderSpec) []proxy.DNSProviderSpec {
	if len(list) == 0 {
		return nil
	}
	out := make([]proxy.DNSProviderSpec, 0, len(list))
	for _, p := range list {
		out = append(out, proxy.DNSProviderSpec{
			Name:        p.Name,
			Kind:        p.Kind,
			Target:      p.Target,
			FallbackIPs: append([]string(nil), p.FallbackIPs...),
			Enabled:     p.Enabled,
			Source:      p.Source,
		})
	}
	return out
}

// StateDir returns the per-user runtime/cache directory for daemon metadata and locks.
func StateDir() (string, error) {
	return hostenv.AppRuntimeDir(hostenv.AppDirName)
}

// StateFilePath returns the path to daemon.json.
func StateFilePath() (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "daemon.json"), nil
}

// ProxyFilePath returns the path to proxy.json for standalone CLI proxy tracking.
func ProxyFilePath() (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "proxy.json"), nil
}

// RegisterStandaloneProxy records the PID and dynamically bound address of a foreground CLI proxy.
func RegisterStandaloneProxy(addr string) error {
	p, err := ProxyFilePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	exe, _ := os.Executable()
	return writeJSONFile(p, ProxySessionInfo{
		PID:  os.Getpid(),
		Addr: addr,
		Exe:  exe,
	})
}

// UnregisterStandaloneProxy removes proxy.json if owned by the current process.
func UnregisterStandaloneProxy() {
	p, err := ProxyFilePath()
	if err != nil {
		return
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var info ProxySessionInfo
	if json.Unmarshal(raw, &info) == nil && info.PID == os.Getpid() {
		_ = os.Remove(p)
	}
}

// FindActiveProxy reports an active agy-patcher bypass proxy owned by either the background daemon or CLI proxy command.
func FindActiveProxy() (addr string, isDaemon bool, ok bool) {
	if _, snap, err := Connect(); err == nil && snap != nil && snap.ProxyOwned && snap.ProxyAddr != "" {
		return snap.ProxyAddr, true, true
	}
	p, err := ProxyFilePath()
	if err != nil {
		return "", false, false
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return "", false, false
	}
	var info ProxySessionInfo
	if err := json.Unmarshal(raw, &info); err != nil || info.PID <= 0 || info.Addr == "" {
		_ = os.Remove(p)
		return "", false, false
	}
	if !IsPIDAlive(info.PID) || !proxy.IsProxyRunning(info.Addr) {
		_ = os.Remove(p)
		return "", false, false
	}
	return info.Addr, false, true
}

// NewService constructs a Service instance ready to bind and serve.
func NewService(opts ServiceOptions) (*Service, error) {
	exe := opts.ExePath
	if exe == "" {
		if p, err := os.Executable(); err == nil {
			exe = p
		}
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil && resolved != "" {
		exe = resolved
	}

	cfg := config.Load()
	lang := opts.Lang
	if lang == "" {
		lang = cfg.Lang
	}
	lang = i18n.NormalizeLang(lang)
	dnsMode, dnsProviders := EffectiveDNS(cfg)

	var tokBuf [32]byte
	if _, err := rand.Read(tokBuf[:]); err != nil {
		return nil, err
	}

	autoOn, _ := autostart.IsEnabled()
	ver := opts.Version
	if ver == "" {
		ver = "1.0.0"
	}

	listenAddr := opts.ListenAddr
	if listenAddr == "" {
		listenAddr = proxy.ListenAddr
	}

	return &Service{
		version:      ver,
		exePath:      exe,
		listenAddr:   listenAddr,
		skipEnv:      opts.SkipEnvPersist,
		lang:         lang,
		token:        hex.EncodeToString(tokBuf[:]),
		cancelRoot:   opts.CancelRoot,
		onChange:     opts.OnChange,
		autostartOn:  autoOn,
		dnsMode:      dnsMode,
		dnsProviders: dnsProviders,
	}, nil
}

// SetOnChange sets or updates the callback invoked when daemon state changes.
func (s *Service) SetOnChange(fn func()) {
	s.mu.Lock()
	s.onChange = fn
	s.mu.Unlock()
}

// SetCancelRoot sets or updates the cancellation function triggered by /v1/shutdown.
func (s *Service) SetCancelRoot(fn context.CancelFunc) {
	s.mu.Lock()
	s.cancelRoot = fn
	s.mu.Unlock()
}

// StartHTTP binds 127.0.0.1:0, serves authenticated daemon control routes, and writes daemon.json.
func (s *Service) StartHTTP(ctx context.Context) (Info, error) {
	statePath, err := StateFilePath()
	if err != nil {
		return Info{}, err
	}
	if err := os.MkdirAll(filepath.Dir(statePath), 0700); err != nil {
		return Info{}, err
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return Info{}, err
	}

	info := Info{
		PID:     os.Getpid(),
		Addr:    ln.Addr().String(),
		Token:   s.token,
		Exe:     s.exePath,
		Version: s.version,
		Lang:    s.lang,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/state", s.authMiddleware(s.handleState))
	mux.HandleFunc("/v1/proxy/start", s.authMiddleware(s.handleProxyStart))
	mux.HandleFunc("/v1/proxy/stop", s.authMiddleware(s.handleProxyStop))
	mux.HandleFunc("/v1/lang", s.authMiddleware(s.handleLang))
	mux.HandleFunc("/v1/dns", s.authMiddleware(s.handleDNS))
	mux.HandleFunc("/v1/autostart/sync", s.authMiddleware(s.handleAutostartSync))
	mux.HandleFunc("/v1/shutdown", s.authMiddleware(s.handleShutdown))

	httpSrv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	s.mu.Lock()
	s.ln = ln
	s.httpSrv = httpSrv
	s.info = info
	s.statePath = statePath
	s.mu.Unlock()

	if err := writeStateFile(statePath, info); err != nil {
		_ = ln.Close()
		return Info{}, err
	}

	go func() {
		<-ctx.Done()
		_ = s.CloseHTTP()
	}()

	go func() {
		_ = httpSrv.Serve(ln)
	}()

	go s.monitorExternalProxy(ctx)

	s.AddLog("info", s.t("daemon_started"))
	return info, nil
}

// CloseHTTP stops the loopback HTTP server and deletes daemon.json if still owned by this instance.
func (s *Service) CloseHTTP() error {
	s.mu.Lock()
	srv := s.httpSrv
	ln := s.ln
	statePath := s.statePath
	info := s.info
	s.httpSrv = nil
	s.ln = nil
	s.mu.Unlock()

	if statePath != "" {
		removeOwnedStateFile(statePath, info)
	}
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = srv.Close()
	} else if ln != nil {
		_ = ln.Close()
	}
	return nil
}

// Shutdown cleanly stops any owned proxy and shuts down the HTTP control endpoint.
func (s *Service) Shutdown() {
	s.StopProxy(false)
	_ = s.CloseHTTP()
}

func (s *Service) Lang() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lang
}

func (s *Service) SetLang(lang string) {
	norm := i18n.NormalizeLang(lang)
	s.mu.Lock()
	changed := s.lang != norm
	s.lang = norm
	s.info.Lang = norm
	statePath := s.statePath
	info := s.info
	s.mu.Unlock()

	if changed {
		if statePath != "" {
			_ = writeStateFile(statePath, info)
		}
		s.notify()
	}
}

func (s *Service) RefreshAutostart() {
	on, _ := autostart.IsEnabled()
	s.mu.Lock()
	s.autostartOn = on
	s.mu.Unlock()
	s.notify()
}

// SetDNS updates custom DNS mode and server priority list, hot-reloading any running proxy.
func (s *Service) SetDNS(mode string, providers []proxy.DNSProviderSpec) {
	normMode := proxy.NormalizeDNSMode(mode)
	list := cloneDNSProviders(providers)

	s.mu.Lock()
	s.dnsMode = normMode
	s.dnsProviders = list
	srv := s.proxyServer
	s.mu.Unlock()

	if srv != nil && srv.Pool != nil {
		srv.Pool.UpdateDNS(normMode, cloneDNSProviders(list))
	}
	s.notify()
}

func (s *Service) t(key string, args ...any) string {
	return i18n.TFor(s.Lang(), key, args...)
}

func (s *Service) monitorExternalProxy(ctx context.Context) {
	refresh := func() {
		s.mu.Lock()
		owned := s.proxyOwned
		s.mu.Unlock()

		ext := ""
		if !owned {
			if fp := env.DetectExternalProxy(); fp != nil {
				ext = fp.Value
			}
		}
		autoOn, _ := autostart.IsEnabled()

		s.mu.Lock()
		changed := s.externalProxy != ext || s.autostartOn != autoOn
		s.externalProxy = ext
		s.autostartOn = autoOn
		s.mu.Unlock()
		if changed {
			s.notify()
		}
	}

	refresh()
	ticker := time.NewTicker(4 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refresh()
		}
	}
}

func (s *Service) notify() {
	s.mu.Lock()
	fn := s.onChange
	s.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// AddLog appends a log event to the daemon's ring buffer.
func (s *Service) AddLog(level, msg string) {
	s.mu.Lock()
	s.nextSeq++
	entry := LogEntry{
		Seq:   s.nextSeq,
		TS:    time.Now().Format("15:04:05"),
		Level: level,
		Msg:   msg,
	}
	s.logs = append(s.logs, entry)
	if len(s.logs) > maxLogEntries {
		s.logs = s.logs[len(s.logs)-maxLogEntries:]
	}
	s.mu.Unlock()
}

// StartProxy starts the embedded local CONNECT bypass proxy if not already active.
func (s *Service) StartProxy() {
	s.mu.Lock()
	if s.proxyOwned || s.shutting || s.proxyCancel != nil {
		s.mu.Unlock()
		return
	}
	listenAddr := s.listenAddr
	lang := s.lang
	skipEnv := s.skipEnv
	dnsMode := s.dnsMode
	dnsProviders := cloneDNSProviders(s.dnsProviders)
	s.mu.Unlock()

	srv := proxy.NewServer(listenAddr, proxy.DohURL)
	srv.Pool.UpdateDNS(dnsMode, dnsProviders)
	if listenAddr == proxy.ListenAddr && !skipEnv {
		if pref := proxy.LoopbackAddrFromURL(env.GetProxyVar()); pref != "" && !proxy.IsProxyRunning(pref) {
			srv.PreferredAddr = pref
		}
	}
	srv.Pool.OnResolve = func(d proxy.ResolveDetail) {
		subTag := "direct"
		if d.Substituted {
			subTag = "sub"
		}
		ipStr := ""
		if len(d.IPs) > 0 {
			ipStr = d.IPs[0]
		}
		s.AddLog("dns", fmt.Sprintf("%s → %s (%s, %s, %dms)", d.Host, ipStr, d.Provider, subTag, d.Latency.Milliseconds()))
	}
	srv.OnConnect = func(host string, gate bool, target string, err error) {
		if err != nil {
			s.AddLog("err", fmt.Sprintf("%s (%s): %v", host, target, err))
			return
		}
		if gate {
			s.AddLog("proxy", fmt.Sprintf("CONNECT %s → %s", host, target))
		}
	}

	pCtx, pCancel := context.WithCancel(context.Background())
	doneCh := make(chan error, 1)

	s.mu.Lock()
	if s.proxyOwned || s.shutting || s.proxyCancel != nil {
		s.mu.Unlock()
		pCancel()
		return
	}
	s.proxyServer = srv
	s.proxyCancel = pCancel
	s.proxyDone = doneCh
	s.proxyNote = i18n.TFor(s.lang, "daemon_proxy_starting")
	s.mu.Unlock()

	s.AddLog("info", i18n.TFor(lang, "daemon_proxy_start_log"))

	readyCh := make(chan string, 1)
	go func() {
		err := srv.Run(pCtx, func(addr string) {
			if !skipEnv {
				if setErr := env.SetProxyVar("http://" + addr); setErr != nil {
					var fpe *env.ForeignProxyError
					if errors.As(setErr, &fpe) {
						s.AddLog("warn", s.t("daemon_proxy_foreign_skipped", fpe.Source, fpe.Value, env.ProxyVar))
					} else {
						s.AddLog("warn", fmt.Sprintf("%s: %v", env.ProxyVar, setErr))
					}
				}
			}
			select {
			case readyCh <- addr:
			default:
			}
		})
		doneCh <- err

		var exitLevel, exitMsg string
		s.mu.Lock()
		wasOwned := s.proxyOwned
		s.proxyOwned = false
		s.proxyAddr = ""
		s.info.ProxyAddr = ""
		s.proxyServer = nil
		s.proxyCancel = nil
		statePath := s.statePath
		info := s.info
		httpAlive := s.httpSrv != nil
		curLang := s.lang
		if err != nil {
			s.proxyNote = fmt.Sprintf("err: %v", err)
			exitLevel = "err"
			exitMsg = i18n.TFor(curLang, "daemon_proxy_err", err)
		} else {
			s.proxyNote = ""
			if wasOwned {
				exitLevel = "info"
				exitMsg = i18n.TFor(curLang, "daemon_proxy_stopped")
			}
		}
		s.mu.Unlock()

		if httpAlive && statePath != "" {
			_ = writeStateFile(statePath, info)
		}
		if exitMsg != "" {
			s.AddLog(exitLevel, exitMsg)
		}
		s.notify()
	}()

	select {
	case addr := <-readyCh:
		s.mu.Lock()
		s.proxyOwned = true
		s.proxyAddr = addr
		s.info.ProxyAddr = addr
		s.proxyNote = ""
		s.externalProxy = ""
		statePath := s.statePath
		info := s.info
		curLang := s.lang
		s.mu.Unlock()
		if statePath != "" {
			_ = writeStateFile(statePath, info)
		}
		s.AddLog("proxy", i18n.TFor(curLang, "daemon_proxy_listening", addr))
	case <-time.After(3 * time.Second):
	}
	s.notify()
}

// StopProxy stops the embedded bypass proxy and removes the dedicated bypass proxy env var unless preserveEnv is true.
func (s *Service) StopProxy(preserveEnv bool) {
	s.mu.Lock()
	cancel := s.proxyCancel
	done := s.proxyDone
	addr := s.proxyAddr
	wasOwned := s.proxyOwned
	skipEnv := s.skipEnv
	lang := s.lang
	statePath := s.statePath
	s.proxyCancel = nil
	s.proxyDone = nil
	s.proxyOwned = false
	s.proxyAddr = ""
	s.info.ProxyAddr = ""
	s.proxyServer = nil
	s.proxyNote = ""
	info := s.info
	httpAlive := s.httpSrv != nil
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	if httpAlive && statePath != "" {
		_ = writeStateFile(statePath, info)
	}
	if !skipEnv && !preserveEnv && addr != "" && env.IsProxyOurs("http://"+addr) {
		_ = env.RemoveProxyVar()
	}
	if wasOwned {
		s.AddLog("info", i18n.TFor(lang, "daemon_proxy_stopped"))
	}
	s.notify()
}

// Snapshot returns the current state snapshot and all buffered log entries with Seq > since.
func (s *Service) Snapshot(since uint64) StateSnapshot {
	s.mu.Lock()
	owned := s.proxyOwned
	addr := s.proxyAddr
	srv := s.proxyServer
	note := s.proxyNote
	ext := s.externalProxy
	autoOn := s.autostartOn
	lang := s.lang
	ver := s.version
	exe := s.exePath
	dnsMode := s.dnsMode
	dnsProviders := cloneDNSProviders(s.dnsProviders)
	nextSeq := s.nextSeq

	var delta []LogEntry
	for _, entry := range s.logs {
		if entry.Seq > since {
			delta = append(delta, entry)
		}
	}
	s.mu.Unlock()

	var stats proxy.Stats
	if owned && srv != nil {
		stats = srv.Stats()
		ext = ""
	}

	return StateSnapshot{
		PID:           os.Getpid(),
		Version:       ver,
		Exe:           exe,
		Lang:          lang,
		ProxyOwned:    owned,
		ProxyAddr:     addr,
		ExternalProxy: ext,
		ProxyNote:     note,
		Stats:         stats,
		DNSMode:       dnsMode,
		DNSProviders:  dnsProviders,
		AutostartOn:   autoOn,
		Logs:          delta,
		NextSeq:       nextSeq,
	}
}

func (s *Service) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Service) handleState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var since uint64
	if raw := r.URL.Query().Get("since"); raw != "" {
		if v, err := strconv.ParseUint(raw, 10, 64); err == nil {
			since = v
		}
	}
	writeJSON(w, s.Snapshot(since))
}

func (s *Service) handleProxyStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.StartProxy()
	writeJSON(w, s.Snapshot(0))
}

func (s *Service) handleProxyStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.StopProxy(false)
	writeJSON(w, s.Snapshot(0))
}

func (s *Service) handleLang(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Lang string `json:"lang"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	s.SetLang(req.Lang)
	writeJSON(w, s.Snapshot(0))
}

func (s *Service) handleAutostartSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.RefreshAutostart()
	writeJSON(w, s.Snapshot(0))
}

func (s *Service) handleDNS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Mode      string                  `json:"mode"`
		Providers []proxy.DNSProviderSpec `json:"providers"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 65536)).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	s.SetDNS(req.Mode, req.Providers)
	writeJSON(w, s.Snapshot(0))
}

func (s *Service) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	s.shutting = true
	cancel := s.cancelRoot
	s.mu.Unlock()

	s.StopProxy(false)
	if s.statePath != "" {
		removeOwnedStateFile(s.statePath, s.info)
	}
	writeJSON(w, map[string]bool{"ok": true})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	if cancel != nil {
		go func() {
			time.Sleep(30 * time.Millisecond)
			cancel()
		}()
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeStateFile(path string, info Info) error {
	return writeJSONFile(path, info)
}

func writeJSONFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), "daemon.*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	_ = os.Chmod(tmpName, 0600)
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func removeOwnedStateFile(path string, own Info) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var cur Info
	if err := json.Unmarshal(raw, &cur); err != nil {
		return
	}
	if cur.PID == own.PID && cur.Token == own.Token {
		_ = os.Remove(path)
	}
}

// SameExecutable reports whether exeA and exeB refer to the same binary on disk (by path or SHA-256).
func SameExecutable(exeA, exeB string) bool {
	if exeA == "" || exeB == "" {
		return false
	}
	ca := filepath.Clean(exeA)
	cb := filepath.Clean(exeB)
	if runtime.GOOS == "windows" {
		if strings.EqualFold(ca, cb) {
			return true
		}
	} else if ca == cb {
		return true
	}
	if ra, err := filepath.EvalSymlinks(ca); err == nil {
		ca = ra
	}
	if rb, err := filepath.EvalSymlinks(cb); err == nil {
		cb = rb
	}
	if runtime.GOOS == "windows" {
		if strings.EqualFold(ca, cb) {
			return true
		}
	} else if ca == cb {
		return true
	}

	ha, errA := fileDigest(ca)
	hb, errB := fileDigest(cb)
	if errA != nil || errB != nil {
		return false
	}
	return bytes.Equal(ha, hb)
}

func fileDigest(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}
