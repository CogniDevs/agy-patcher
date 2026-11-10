package rupack

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"agy-patcher/pkg/discovery"
)

func buildTestAsar(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	type fileMeta struct {
		Offset string `json:"offset"`
		Size   int64  `json:"size"`
	}
	type dirNode struct {
		Files map[string]any `json:"files"`
	}

	root := dirNode{Files: make(map[string]any)}
	var payload bytes.Buffer
	addEntry := func(rel string, content []byte) {
		off := payload.Len()
		payload.Write(content)
		meta := fileMeta{
			Offset: strconv.Itoa(off),
			Size:   int64(len(content)),
		}
		cur := root.Files
		segs := splitSlash(rel)
		for i, seg := range segs {
			if i == len(segs)-1 {
				cur[seg] = meta
			} else {
				sub, ok := cur[seg].(dirNode)
				if !ok {
					sub = dirNode{Files: make(map[string]any)}
					cur[seg] = sub
				}
				cur = sub.Files
			}
		}
	}
	for k, v := range files {
		addEntry(k, v)
	}

	hdrJSON, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	pad := (4 - (len(hdrJSON) % 4)) % 4
	hdrStringSize := uint32(len(hdrJSON))
	paddedJSON := append(hdrJSON, bytes.Repeat([]byte{0}, pad)...)
	headerSize := uint32(len(paddedJSON) + 8)
	pickleSize := uint32(len(paddedJSON) + 4)

	var out bytes.Buffer
	_ = binary.Write(&out, binary.LittleEndian, uint32(4))
	_ = binary.Write(&out, binary.LittleEndian, headerSize)
	_ = binary.Write(&out, binary.LittleEndian, pickleSize)
	_ = binary.Write(&out, binary.LittleEndian, hdrStringSize)
	out.Write(paddedJSON)
	out.Write(payload.Bytes())
	return out.Bytes()
}

func splitSlash(s string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	parts = append(parts, s[start:])
	return parts
}

func buildMockArchive(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestInstallReapplyAndUninstall(t *testing.T) {
	tmp := t.TempDir()
	resDir := filepath.Join(tmp, "resources")
	if err := os.MkdirAll(resDir, 0755); err != nil {
		t.Fatal(err)
	}

	origAsar := buildTestAsar(t, map[string][]byte{
		"package.json":           []byte(`{"version":"2.31.0"}`),
		"dist/languageServer.js": []byte(`args.push("--original");`),
	})
	ruAsar := buildTestAsar(t, map[string][]byte{
		"package.json":           []byte(`{"version":"2.31.0"}`),
		"dist/languageServer.js": []byte(`args.push("--web_bundle_path=web_bundle_ru");`),
	})
	oldAsar := buildTestAsar(t, map[string][]byte{
		"package.json":           []byte(`{"version":"2.26.0"}`),
		"dist/languageServer.js": []byte(`args.push("--web_bundle_path=web_bundle_ru");`),
	})

	targetAsarPath := filepath.Join(resDir, "app.asar")
	if err := os.WriteFile(targetAsarPath, origAsar, 0644); err != nil {
		t.Fatal(err)
	}

	zipBytes := buildMockArchive(t, map[string][]byte{
		"antigravity-ru-main/resources/2.26.0/app.asar":                 oldAsar,
		"antigravity-ru-main/resources/2.26.0/web_bundle_ru/index.html": []byte("old"),
		"antigravity-ru-main/resources/2.31.0/app.asar":                 ruAsar,
		"antigravity-ru-main/resources/2.31.0/web_bundle_ru/index.html": []byte(`<script src="/i18n-ru.js"></script>`),
		"antigravity-ru-main/resources/2.31.0/web_bundle_ru/i18n-ru.js": []byte(`console.log("ru");`),
		"antigravity-ru-main/README.md":                                 []byte("ignored"),
	})

	apiHits := 0
	zipHits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api":
			apiHits++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"name":"2.26.0","type":"dir"},{"name":"2.31.0","type":"dir"}]`))
		case "/main.zip":
			zipHits++
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(zipBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	inst := discovery.Install{
		Type:         discovery.TypeDesktop,
		Path:         tmp,
		ResourcesDir: resDir,
		AsarPath:     targetAsarPath,
	}

	before := InspectDesktop(inst)
	if !before.Applicable || before.Version != "2.31.0" || before.Installed || before.HasBackup {
		t.Fatalf("unexpected initial InspectDesktop: %+v", before)
	}

	opts := InstallOptions{
		APIURL: srv.URL + "/api",
		ZipURL: srv.URL + "/main.zip",
		Client: srv.Client(),
	}
	res, err := Install(context.Background(), inst, opts)
	if err != nil {
		t.Fatalf("Install failed: %v", err)
	}
	if !res.BackupCreated || res.PackVersion != "2.31.0" || apiHits != 1 || zipHits != 1 {
		t.Fatalf("unexpected install result: %+v (api=%d zip=%d)", res, apiHits, zipHits)
	}

	after := InspectDesktop(inst)
	if !after.Installed || !after.HasBackup {
		t.Fatalf("expected Installed && HasBackup after Install, got %+v", after)
	}

	// Verify only 2.31.0 bundle files were extracted.
	if raw, err := os.ReadFile(filepath.Join(resDir, BundleDirName, "index.html")); err != nil || !bytes.Contains(raw, []byte("i18n-ru.js")) {
		t.Fatalf("unexpected web_bundle_ru/index.html: %q err=%v", string(raw), err)
	}

	// Re-install must keep original backup intact rather than overwriting with ruAsar.
	res2, err := Install(context.Background(), inst, opts)
	if err != nil {
		t.Fatalf("second Install failed: %v", err)
	}
	if res2.BackupCreated || !res2.BackupKept {
		t.Fatalf("expected original backup to be preserved on reinstall, got %+v", res2)
	}
	bakBytes, _ := os.ReadFile(filepath.Join(resDir, BackupAsarName))
	if !bytes.Equal(bakBytes, origAsar) {
		t.Fatalf("original backup was overwritten")
	}

	// Uninstall restores exact original app.asar and deletes backup & web_bundle_ru.
	uRes, err := Uninstall(inst, nil)
	if err != nil {
		t.Fatalf("Uninstall failed: %v", err)
	}
	if !uRes.AsarRestored || !uRes.BundleRemoved {
		t.Fatalf("unexpected uninstall result: %+v", uRes)
	}
	curBytes, _ := os.ReadFile(targetAsarPath)
	if !bytes.Equal(curBytes, origAsar) {
		t.Fatalf("restored app.asar differs from original")
	}
	final := InspectDesktop(inst)
	if final.Installed || final.HasBackup {
		t.Fatalf("expected clean state after Uninstall, got %+v", final)
	}
}

func TestUnsupportedVersionSkipsZipDownloadAndForceWorks(t *testing.T) {
	tmp := t.TempDir()
	resDir := filepath.Join(tmp, "resources")
	_ = os.MkdirAll(resDir, 0755)
	origAsar := buildTestAsar(t, map[string][]byte{
		"package.json":           []byte(`{"version":"2.99.0"}`),
		"dist/languageServer.js": []byte(`args.push("--original");`),
	})
	ruAsar := buildTestAsar(t, map[string][]byte{
		"package.json":           []byte(`{"version":"2.31.0"}`),
		"dist/languageServer.js": []byte(`args.push("--web_bundle_path=web_bundle_ru");`),
	})
	_ = os.WriteFile(filepath.Join(resDir, "app.asar"), origAsar, 0644)

	zipBytes := buildMockArchive(t, map[string][]byte{
		"antigravity-ru-main/resources/2.31.0/app.asar":                 ruAsar,
		"antigravity-ru-main/resources/2.31.0/web_bundle_ru/index.html": []byte("ru"),
	})
	zipDownloads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api":
			_, _ = w.Write([]byte(`[{"name":"2.31.0","type":"dir"}]`))
		case "/main.zip":
			zipDownloads++
			_, _ = w.Write(zipBytes)
		}
	}))
	defer srv.Close()

	inst := discovery.Install{Type: discovery.TypeDesktop, Path: tmp, ResourcesDir: resDir, AsarPath: filepath.Join(resDir, "app.asar")}
	_, err := Install(context.Background(), inst, InstallOptions{
		APIURL: srv.URL + "/api",
		ZipURL: srv.URL + "/main.zip",
		Client: srv.Client(),
	})
	var uve *UnsupportedVersionError
	if !errors.As(err, &uve) {
		t.Fatalf("expected UnsupportedVersionError, got %v", err)
	}
	if zipDownloads != 0 {
		t.Fatalf("expected API precheck to prevent zip download, got %d downloads", zipDownloads)
	}

	res, err := Install(context.Background(), inst, InstallOptions{
		Force:  true,
		APIURL: srv.URL + "/api",
		ZipURL: srv.URL + "/main.zip",
		Client: srv.Client(),
	})
	if err != nil || res.PackVersion != "2.31.0" {
		t.Fatalf("expected Force install of 2.31.0, got res=%+v err=%v", res, err)
	}
}

func TestZipSlipRejected(t *testing.T) {
	zrBytes := buildMockArchive(t, map[string][]byte{
		"antigravity-ru-main/resources/2.31.0/web_bundle_ru/../../evil.txt": []byte("pwn"),
		"antigravity-ru-main/resources/2.31.0/app.asar":                     []byte("x"),
	})
	r, err := zip.NewReader(bytes.NewReader(zrBytes), int64(len(zrBytes)))
	if err != nil {
		t.Fatal(err)
	}
	if err := extractVersionFromZip(r.File, "2.31.0", t.TempDir()); err == nil {
		t.Fatalf("expected zip-slip path to be rejected")
	}
}
