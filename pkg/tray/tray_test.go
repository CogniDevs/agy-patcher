package tray

import (
	"testing"

	"agy-patcher/pkg/assets"
)

func TestAssetsNonEmpty(t *testing.T) {
	if len(assets.IconICO) == 0 || len(assets.TrayOnICO) == 0 || len(assets.TrayOffICO) == 0 || len(assets.IconPNG) == 0 {
		t.Fatalf("expected embedded tray and icon assets to be non-empty")
	}
}
