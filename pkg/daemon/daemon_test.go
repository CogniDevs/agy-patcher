package daemon

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agy-patcher/pkg/config"
	"agy-patcher/pkg/proxy"
)

func TestServiceHTTPStateLogsAndShutdown(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)
	t.Setenv("XDG_RUNTIME_DIR", tmp)
	t.Setenv("XDG_CACHE_HOME", tmp)
	t.Setenv("HOME", tmp)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	svc, err := NewService(ServiceOptions{
		Version:    "1.2.3",
		Lang:       "ru",
		ExePath:    os.Args[0],
		CancelRoot: cancel,
	})
	if err != nil {
		t.Fatalf("NewService failed: %v", err)
	}
	defer svc.Shutdown()

	info, err := svc.StartHTTP(ctx)
	if err != nil {
		t.Fatalf("StartHTTP failed: %v", err)
	}
	if info.Addr == "" || info.Token == "" {
		t.Fatalf("expected non-empty addr and token")
	}

	// Unauthenticated request must fail with 401.
	resp, err := http.Get("http://" + info.Addr + "/v1/state")
	if err != nil {
		t.Fatalf("GET /v1/state failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", resp.StatusCode)
	}

	cl, snap, err := Connect()
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	if snap.Version != "1.2.3" || snap.Lang != "ru" || len(snap.Logs) == 0 {
		t.Fatalf("unexpected initial snapshot: %+v", snap)
	}

	seq := snap.NextSeq
	svc.AddLog("dns", "aicode.googleapis.com -> 1.2.3.4")

	snap2, err := cl.State(seq)
	if err != nil {
		t.Fatalf("State(seq) failed: %v", err)
	}
	if len(snap2.Logs) != 1 || snap2.Logs[0].Level != "dns" {
		t.Fatalf("expected 1 incremental dns log, got %+v", snap2.Logs)
	}

	snap3, err := cl.SetLang("en")
	if err != nil {
		t.Fatalf("SetLang failed: %v", err)
	}
	if snap3.Lang != "en" {
		t.Fatalf("expected lang 'en', got %q", snap3.Lang)
	}
	if snap3Logs, err := cl.State(seq); err == nil {
		for _, entry := range snap3Logs.Logs {
			if entry.Msg == "Interface language: English" || entry.Msg == "Язык интерфейса: Русский" {
				t.Fatalf("unexpected language-switch log entry: %+v", entry)
			}
		}
	}

	customDNS := []proxy.DNSProviderSpec{
		{Name: "custom.doh", Kind: proxy.DNSKindDoH, Target: "https://dns.example/dns-query", FallbackIPs: []string{"1.2.3.4"}, Enabled: true},
		{Name: "udp-1", Kind: proxy.DNSKindUDP, Target: "9.9.9.9:53", Enabled: false},
	}
	snap4, err := cl.SetDNS(proxy.DNSModeStrict, customDNS)
	if err != nil {
		t.Fatalf("SetDNS failed: %v", err)
	}
	if snap4.DNSMode != proxy.DNSModeStrict || len(snap4.DNSProviders) != 2 || snap4.DNSProviders[0].Name != "custom.doh" || snap4.DNSProviders[1].Enabled {
		t.Fatalf("unexpected DNS snapshot after SetDNS: mode=%q providers=%+v", snap4.DNSMode, snap4.DNSProviders)
	}

	if err := cl.Shutdown(); err != nil {
		// Shutdown may return nil and cancel context cleanly.
		t.Fatalf("Shutdown failed: %v", err)
	}

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatalf("expected Shutdown to cancel daemon root context")
	}
}

func TestEffectiveDNSUserSeparationAndQuarantine(t *testing.T) {
	cfg := config.Config{
		DNSMode:       proxy.DNSModeAuto,
		DNSCustomized: true,
		DNSProviders: []config.DNSProvider{
			{Name: "cat-ok", Kind: "doh", Target: "https://ok.cat/dns-query", Enabled: true, Source: proxy.DNSSourceCatalog},
			{Name: "cat-bad", Kind: "udp", Target: "203.0.113.9:53", Enabled: true, Source: proxy.DNSSourceCatalog},
		},
		UserDNSProviders: []config.DNSProvider{
			{Name: "user-only", Kind: "dot", Target: "tls://user.dns.test", Enabled: true},
		},
		HiddenDNSKeys: []string{"udp|203.0.113.9:53", "dot|tls://user.dns.test"},
	}

	_, eff := EffectiveDNS(cfg)
	if len(eff) != 2 {
		t.Fatalf("expected hidden catalog skipped and user-only preserved (len=2), got %+v", eff)
	}
	if eff[0].Target != "https://ok.cat/dns-query" || eff[1].Target != "tls://user.dns.test" || eff[1].Source != proxy.DNSSourceUser {
		t.Fatalf("unexpected effective DNS order/sources: %+v", eff)
	}
}

func TestServiceStartStopProxyConcurrentNoDeadlock(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)
	t.Setenv("XDG_RUNTIME_DIR", tmp)
	t.Setenv("XDG_CACHE_HOME", tmp)
	t.Setenv("HOME", tmp)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var svc *Service
	var err error
	svc, err = NewService(ServiceOptions{
		Version:        "1.0.0",
		Lang:           "ru",
		ExePath:        os.Args[0],
		ListenAddr:     "127.0.0.1:0",
		SkipEnvPersist: true,
		CancelRoot:     cancel,
		OnChange: func() {
			// Tray WndProc calls Snapshot(0) synchronously upon notification.
			_ = svc.Snapshot(0)
		},
	})
	if err != nil {
		t.Fatalf("NewService failed: %v", err)
	}
	defer svc.Shutdown()

	if _, err := svc.StartHTTP(ctx); err != nil {
		t.Fatalf("StartHTTP failed: %v", err)
	}

	cl, _, err := Connect()
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		snap, startErr := cl.StartProxy()
		if startErr != nil {
			done <- startErr
			return
		}
		if !snap.ProxyOwned || snap.ProxyAddr == "" {
			done <- http.ErrServerClosed
			return
		}
		if recorded, rErr := ReadStateFile(); rErr != nil || recorded.ProxyAddr != snap.ProxyAddr {
			done <- http.ErrMissingFile
			return
		}
		if _, langErr := cl.SetLang("en"); langErr != nil {
			done <- langErr
			return
		}
		snap2, stopErr := cl.StopProxy()
		if stopErr != nil {
			done <- stopErr
			return
		}
		if snap2.ProxyOwned {
			done <- http.ErrAbortHandler
			return
		}
		if recordedAfter, rErr := ReadStateFile(); rErr != nil || recordedAfter.ProxyAddr != "" {
			done <- http.ErrHandlerTimeout
			return
		}
		done <- nil
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("concurrent proxy start/stop failed: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatalf("deadlock/timeout during StartProxy -> Snapshot -> StopProxy cycle")
	}
}

func TestSameExecutable(t *testing.T) {
	tmp := t.TempDir()
	a := filepath.Join(tmp, "a.exe")
	b := filepath.Join(tmp, "b.exe")
	c := filepath.Join(tmp, "c.exe")
	_ = os.WriteFile(a, []byte("same-bytes"), 0755)
	_ = os.WriteFile(b, []byte("same-bytes"), 0755)
	_ = os.WriteFile(c, []byte("diff-bytes"), 0755)

	if !SameExecutable(a, a) {
		t.Fatalf("expected SameExecutable(a, a) to be true")
	}
	if !SameExecutable(a, b) {
		t.Fatalf("expected SameExecutable(a, b) with identical bytes to be true")
	}
	if SameExecutable(a, c) {
		t.Fatalf("expected SameExecutable(a, c) with different bytes to be false")
	}
}

func TestInstanceLock(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)
	t.Setenv("XDG_RUNTIME_DIR", tmp)
	t.Setenv("XDG_CACHE_HOME", tmp)
	t.Setenv("HOME", tmp)

	// Avoid colliding with any user-session Windows mutex during unit test by only checking flock on Unix,
	// or checking Release cleans cleanly on Unix.
	lock, err := AcquireLock()
	if err == nil {
		if !IsLocked() {
			lock.Release()
			t.Fatalf("expected IsLocked() == true while held")
		}
		lock.Release()
	}
}
