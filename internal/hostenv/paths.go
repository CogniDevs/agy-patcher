package hostenv

import (
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
)

// AppDirName is the canonical folder name used across per-user config, cache, and install directories.
const AppDirName = "agpatcher"

// EffectiveHomeDir resolves the active user's home directory, preferring SUDO_USER when running elevated on Unix.
func EffectiveHomeDir() string {
	if runtime.GOOS != "windows" && os.Geteuid() == 0 {
		if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" && sudoUser != "root" {
			if u, err := user.Lookup(sudoUser); err == nil && u.HomeDir != "" {
				return u.HomeDir
			}
		}
	}
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	if h := os.Getenv("USERPROFILE"); h != "" {
		return h
	}
	return os.Getenv("HOME")
}

// XDGConfigHome returns the Unix XDG config base directory with fallback to ~/.config.
func XDGConfigHome() string {
	if runtime.GOOS != "windows" && os.Geteuid() == 0 && os.Getenv("SUDO_USER") != "" {
		if home := EffectiveHomeDir(); home != "" {
			return filepath.Join(home, ".config")
		}
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return xdg
	}
	if home := EffectiveHomeDir(); home != "" {
		return filepath.Join(home, ".config")
	}
	return ""
}

// XDGDataHome returns the Unix XDG data base directory with fallback to ~/.local/share.
func XDGDataHome() string {
	if runtime.GOOS != "windows" && os.Geteuid() == 0 && os.Getenv("SUDO_USER") != "" {
		if home := EffectiveHomeDir(); home != "" {
			return filepath.Join(home, ".local", "share")
		}
	}
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return xdg
	}
	if home := EffectiveHomeDir(); home != "" {
		return filepath.Join(home, ".local", "share")
	}
	return ""
}

// XDGCacheHome returns the Unix XDG cache base directory with fallback to ~/.cache.
func XDGCacheHome() string {
	if xdg := os.Getenv("XDG_CACHE_HOME"); xdg != "" {
		return xdg
	}
	if home := EffectiveHomeDir(); home != "" {
		return filepath.Join(home, ".cache")
	}
	return ""
}

func windowsLocalAppData() (string, error) {
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		return local, nil
	}
	cfg, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(cfg), "Local"), nil
}

// AppConfigDir returns the OS-specific configuration directory for appName.
func AppConfigDir(appName string) (string, error) {
	switch runtime.GOOS {
	case "windows":
		local, err := windowsLocalAppData()
		if err != nil {
			return "", err
		}
		return filepath.Join(local, appName), nil
	case "darwin":
		home := EffectiveHomeDir()
		if home == "" {
			return os.UserConfigDir()
		}
		return filepath.Join(home, "Library", "Application Support", appName), nil
	default:
		base := XDGConfigHome()
		if base == "" {
			return os.UserConfigDir()
		}
		return filepath.Join(base, appName), nil
	}
}

// AppDataDir returns the OS-specific persistent application data directory for appName.
func AppDataDir(appName string) (string, error) {
	switch runtime.GOOS {
	case "windows":
		local, err := windowsLocalAppData()
		if err != nil {
			return "", err
		}
		return filepath.Join(local, appName), nil
	case "darwin":
		home := EffectiveHomeDir()
		if home == "" {
			return os.UserConfigDir()
		}
		return filepath.Join(home, "Library", "Application Support", appName), nil
	default:
		base := XDGDataHome()
		if base == "" {
			return os.UserConfigDir()
		}
		return filepath.Join(base, appName), nil
	}
}

// AppRuntimeDir returns the OS-specific runtime/cache state directory for appName.
func AppRuntimeDir(appName string) (string, error) {
	switch runtime.GOOS {
	case "windows":
		local, err := windowsLocalAppData()
		if err != nil {
			return "", err
		}
		return filepath.Join(local, appName), nil
	case "darwin":
		if cache, err := os.UserCacheDir(); err == nil && cache != "" {
			return filepath.Join(cache, appName), nil
		}
		home := EffectiveHomeDir()
		if home == "" {
			return os.UserCacheDir()
		}
		return filepath.Join(home, "Library", "Caches", appName), nil
	default:
		if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
			return filepath.Join(rt, appName), nil
		}
		if cache, err := os.UserCacheDir(); err == nil && cache != "" {
			return filepath.Join(cache, appName), nil
		}
		base := XDGCacheHome()
		if base == "" {
			return os.UserCacheDir()
		}
		return filepath.Join(base, appName), nil
	}
}

// IDEConfigRoot returns the VS Code/Electron IDE configuration root directory.
func IDEConfigRoot() string {
	switch runtime.GOOS {
	case "windows":
		return os.Getenv("APPDATA")
	case "darwin":
		if home := EffectiveHomeDir(); home != "" {
			return filepath.Join(home, "Library", "Application Support")
		}
		return ""
	default:
		return XDGConfigHome()
	}
}

// NormalizePathKey produces a clean OS-appropriate lookup key for path deduplication.
func NormalizePathKey(p string) string {
	if p == "" {
		return ""
	}
	cleaned := filepath.Clean(p)
	if runtime.GOOS == "windows" {
		return strings.ToLower(cleaned)
	}
	return cleaned
}

// SamePath compares two filesystem paths for equivalence on the host OS.
func SamePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return NormalizePathKey(a) == NormalizePathKey(b)
}
