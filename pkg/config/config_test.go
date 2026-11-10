package config

import (
	"path/filepath"
	"testing"
)

func TestSaveLoadAndRemoveConfig(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("HOME", tmp)

	want := Config{
		Lang:        "en",
		ManualPaths: []string{`D:\Custom\Antigravity`},
		ReplacedPaths: map[string]string{
			"desktop|c:\\ag": `D:\Moved\Antigravity`,
		},
		Deselected:    []string{"desktop|c:\\ag"},
		DNSMode:       "strict",
		DNSCatalogURL: "https://example.invalid/providers.json",
		DNSCustomized: true,
		DNSProviders: []DNSProvider{
			{Name: "custom.doh", Kind: "doh", Target: "https://dns.example/dns-query", FallbackIPs: []string{"1.2.3.4"}, Enabled: true, Source: "user"},
			{Name: "udp-1", Kind: "udp", Target: "9.9.9.9:53", Enabled: false, Source: "catalog"},
		},
		UserDNSProviders: []DNSProvider{
			{Name: "custom.doh", Kind: "doh", Target: "https://dns.example/dns-query", FallbackIPs: []string{"1.2.3.4"}, Enabled: true, Source: "user"},
		},
		HiddenDNSKeys: []string{"udp|9.9.9.9:53"},
	}
	if err := Save(want); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	got := Load()
	if got.Lang != "en" || len(got.ManualPaths) != 1 || got.ManualPaths[0] != want.ManualPaths[0] || len(got.Deselected) != 1 {
		t.Fatalf("unexpected loaded config: %+v", got)
	}
	if got.ReplacedPaths["desktop|c:\\ag"] != `D:\Moved\Antigravity` {
		t.Fatalf("expected replaced path persisted, got %+v", got.ReplacedPaths)
	}
	if got.DNSMode != "strict" || !got.DNSCustomized || len(got.DNSProviders) != 2 || got.DNSProviders[1].Enabled {
		t.Fatalf("expected custom DNS persisted, got mode=%q custom=%v providers=%+v", got.DNSMode, got.DNSCustomized, got.DNSProviders)
	}
	if got.DNSCatalogURL != want.DNSCatalogURL || len(got.UserDNSProviders) != 1 || got.UserDNSProviders[0].Source != "user" || len(got.HiddenDNSKeys) != 1 {
		t.Fatalf("expected separated user DNS / catalog URL / hidden keys persisted, got %+v", got)
	}

	// Verify SaveTo/LoadFrom normalization
	p := filepath.Join(tmp, "sub", "cfg.json")
	if err := SaveTo(p, Config{Lang: "FR"}); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadFrom(p)
	if err != nil || loaded.Lang != "" {
		t.Fatalf("expected invalid lang normalized to empty, got %+v, err=%v", loaded, err)
	}

	if err := Remove(); err != nil {
		t.Fatalf("Remove failed: %v", err)
	}
	if after := Load(); after.Lang != "" || len(after.ManualPaths) != 0 {
		t.Fatalf("expected empty config after Remove, got %+v", after)
	}
}
