package env

import "testing"

func TestIsSameProxyAddr(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"http://127.0.0.1:38451", "http://127.0.0.1:38451/", true},
		{"HTTP://127.0.0.1:38451", "127.0.0.1:38451", true},
		{"http://127.0.0.1:10808", "http://127.0.0.1:38451", false},
		{"", "http://127.0.0.1:38451", false},
	}

	for _, tc := range cases {
		if got := isSameProxyAddr(tc.a, tc.b); got != tc.want {
			t.Errorf("isSameProxyAddr(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestParseEnvFileVar(t *testing.T) {
	content := "# agy-patcher bypass proxy\n# comment\nAGY_PROXY_U=http://127.0.0.1:38451\n"
	if got := parseEnvFileVar(content, ProxyVar); got != "http://127.0.0.1:38451" {
		t.Fatalf("unexpected parsed var: %q", got)
	}
}

func TestProxyVarLengthMatchesHttpsProxy(t *testing.T) {
	if len(ProxyVar) != len("https_proxy") {
		t.Fatalf("expected len(%q) == %d, got %d", ProxyVar, len("https_proxy"), len(ProxyVar))
	}
}

func TestForeignProxyDetectsProcessEnv(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:8888")
	fp := DetectExternalProxy()
	if fp == nil || fp.Value != "http://127.0.0.1:8888" {
		t.Fatalf("expected foreign proxy detected, got %+v", fp)
	}
}
