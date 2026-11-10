package i18n

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestInitAndFallback(t *testing.T) {
	if err := Init("ru_RU.UTF-8", ""); err != nil {
		t.Fatalf("Init ru failed: %v", err)
	}
	if got := Get("cmd_commands"); got != "Команды (при запуске без аргументов открывается интерактивная панель):" {
		t.Fatalf("expected Russian translation, got %q", got)
	}

	if got := Get("nonexistent_key_xyz"); got != "nonexistent_key_xyz" {
		t.Fatalf("expected missing key fallback, got %q", got)
	}

	// Unknown language normalizes to en.
	if err := Init("fr_FR", ""); err != nil {
		t.Fatalf("Init fr failed: %v", err)
	}
	if got := Get("cmd_commands"); got != "Commands (run without arguments to open interactive console UI):" {
		t.Fatalf("expected English fallback, got %q", got)
	}

	SetLanguage("ru")
	if got := Get("status_title"); got != "Статус:" {
		t.Fatalf("expected Russian after SetLanguage, got %q", got)
	}
	if got := TFor("en", "tui_label_os"); got != "OS: " {
		t.Fatalf("TFor explicit language failed: got %q", got)
	}
}

func TestDiskOverlay(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "en.json"), []byte(`{"status_title":"Custom:"}`), 0644); err != nil {
		t.Fatal(err)
	}

	if err := Init("en", dir); err != nil {
		t.Fatalf("Init with overlay failed: %v", err)
	}
	if got := Get("status_title"); got != "Custom:" {
		t.Fatalf("expected overlaid value, got %q", got)
	}
	// Non-overridden keys remain available from embedded FS.
	if got := Get("cmd_commands"); got != "Commands (run without arguments to open interactive console UI):" {
		t.Fatalf("expected embedded key, got %q", got)
	}
}

var reFmtVerb = regexp.MustCompile(`%[sdvqxf]`)

func TestEmbeddedLocalesParityAndPlaceholders(t *testing.T) {
	loaded := loadEmbeddedDefaults()
	ruMap := loaded["ru"]
	enMap := loaded["en"]
	if len(ruMap) == 0 || len(enMap) == 0 {
		t.Fatalf("embedded locales cannot be empty (ru=%d en=%d)", len(ruMap), len(enMap))
	}

	for k, vRU := range ruMap {
		if strings.TrimSpace(vRU) == "" {
			t.Errorf("ru.json key %q is empty", k)
		}
		vEN, ok := enMap[k]
		if !ok {
			t.Errorf("key %q missing in en.json", k)
			continue
		}
		verbsRU := reFmtVerb.FindAllString(vRU, -1)
		verbsEN := reFmtVerb.FindAllString(vEN, -1)
		if strings.Join(verbsRU, ",") != strings.Join(verbsEN, ",") {
			t.Errorf("format verbs mismatch for %q: ru=%v en=%v", k, verbsRU, verbsEN)
		}
	}
	for k, vEN := range enMap {
		if strings.TrimSpace(vEN) == "" {
			t.Errorf("en.json key %q is empty", k)
		}
		if _, ok := ruMap[k]; !ok {
			t.Errorf("key %q missing in ru.json", k)
		}
	}
}
