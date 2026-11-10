package i18n

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"agy-patcher/locales"
)

var (
	translations map[string]map[string]string
	currentLang  = "en"
	mu           sync.RWMutex
	defaultOnce  sync.Once
)

var supportedLanguages = []string{"ru", "en"}

// NormalizeLang normalizes locale strings (e.g. "ru_RU.UTF-8", "ru-RU") to a supported language code.
func NormalizeLang(lang string) string {
	lower := strings.ToLower(strings.TrimSpace(lang))
	if strings.HasPrefix(lower, "ru") {
		return "ru"
	}
	return "en"
}

func loadEmbeddedDefaults() map[string]map[string]string {
	loaded := make(map[string]map[string]string, len(supportedLanguages))
	for _, l := range supportedLanguages {
		langMap := make(map[string]string)
		if data, err := fs.ReadFile(locales.FS, l+".json"); err == nil {
			_ = json.Unmarshal(data, &langMap)
		}
		loaded[l] = langMap
	}
	return loaded
}

func ensureLoaded() {
	mu.RLock()
	ready := translations != nil
	mu.RUnlock()
	if ready {
		return
	}
	defaultOnce.Do(func() {
		defaults := loadEmbeddedDefaults()
		mu.Lock()
		if translations == nil {
			translations = defaults
		}
		mu.Unlock()
	})
}

// Init loads translations from embedded locale files and optionally overlays files from localesDir.
func Init(lang string, localesDir string) error {
	loaded := make(map[string]map[string]string, len(supportedLanguages))

	for _, l := range supportedLanguages {
		langMap := make(map[string]string)

		if data, err := fs.ReadFile(locales.FS, l+".json"); err == nil {
			if err := json.Unmarshal(data, &langMap); err != nil {
				return fmt.Errorf("error parsing embedded %s.json: %w", l, err)
			}
		}

		if localesDir != "" {
			diskPath := filepath.Join(localesDir, l+".json")
			if data, err := os.ReadFile(diskPath); err == nil {
				var diskMap map[string]string
				if err := json.Unmarshal(data, &diskMap); err != nil {
					return fmt.Errorf("error parsing %s: %w", diskPath, err)
				}
				for k, v := range diskMap {
					langMap[k] = v
				}
			}
		}

		loaded[l] = langMap
	}

	mu.Lock()
	defer mu.Unlock()
	translations = loaded
	currentLang = NormalizeLang(lang)
	return nil
}

// Get returns the localized string for key using the active global language, falling back to English.
func Get(key string) string {
	mu.RLock()
	lang := currentLang
	mu.RUnlock()
	return GetFor(lang, key)
}

// T formats key with args using the active global language.
func T(key string, args ...any) string {
	tmpl := Get(key)
	if len(args) == 0 {
		return tmpl
	}
	return fmt.Sprintf(tmpl, args...)
}

// GetFor returns the localized string for key in an explicit language ("ru" or "en"), falling back to English.
func GetFor(lang, key string) string {
	ensureLoaded()
	norm := NormalizeLang(lang)

	mu.RLock()
	defer mu.RUnlock()

	if translations == nil {
		return key
	}
	if langMap, ok := translations[norm]; ok {
		if val, ok := langMap[key]; ok {
			return val
		}
	}
	if norm != "en" {
		if enMap, ok := translations["en"]; ok {
			if val, ok := enMap[key]; ok {
				return val
			}
		}
	}
	return key
}

// TFor formats key with args in an explicit language ("ru" or "en").
func TFor(lang, key string, args ...any) string {
	tmpl := GetFor(lang, key)
	if len(args) == 0 {
		return tmpl
	}
	return fmt.Sprintf(tmpl, args...)
}

// SetLanguage updates the active global language.
func SetLanguage(lang string) {
	ensureLoaded()
	norm := NormalizeLang(lang)
	mu.Lock()
	defer mu.Unlock()
	if translations == nil {
		currentLang = norm
		return
	}
	if _, ok := translations[norm]; ok {
		currentLang = norm
	}
}
