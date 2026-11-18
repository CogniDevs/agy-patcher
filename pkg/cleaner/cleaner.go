package cleaner

import (
	"errors"
	"path/filepath"
	"sync"

	"agy-patcher/internal/hostenv"
	"agy-patcher/pkg/autostart"
	"agy-patcher/pkg/daemon"
	"agy-patcher/pkg/discovery"
	"agy-patcher/pkg/env"
	"agy-patcher/pkg/patcher"
	"agy-patcher/pkg/rupack"
)

var AntigravityHelperNames = []string{
	"language_server.exe",
	"language_server_windows_x64.exe",
	"language_server",
	"language_server_linux_x64",
	"language_server_darwin_arm64",
	"language_server_darwin_x64",
	"agy.exe",
	"agy",
	"Antigravity CLI.exe",
	"antigravity-sidecar.exe",
	"antigravity-sidecar",
}

var AntigravityAppNames = []string{
	"Antigravity.exe",
	"Antigravity",
	"Antigravity IDE.exe",
	"Antigravity IDE",
	"Antigravity CLI.exe",
	"agy.exe",
	"agy",
	"language_server.exe",
	"language_server_windows_x64.exe",
	"language_server",
	"antigravity-sidecar.exe",
	"antigravity-sidecar",
}

// DisableProcessKillForTest disables OS process termination during unit tests.
var DisableProcessKillForTest = false

// CleanOrphanAntigravityProcesses finds and terminates orphaned Sidecar SDK and language server helpers
// whose parent application exited. Helpers with a live parent are left running.
func CleanOrphanAntigravityProcesses() int {
	if DisableProcessKillForTest {
		return 0
	}
	return hostenv.TerminateOrphanProcesses(AntigravityHelperNames)
}

// CloseAntigravityProcesses terminates running Antigravity Desktop/IDE and Sidecar helper processes
// to release file locks before patching or complete rollback.
func CloseAntigravityProcesses() int {
	if DisableProcessKillForTest {
		return 0
	}
	return hostenv.CloseProcessesByNames(AntigravityAppNames)
}

// FileResult records the cleanup outcome for a single target file, archive, or localization bundle.
type FileResult struct {
	Path     string
	Restored bool
	Err      error
}

// Report describes all actions performed by CleanInstalls and DeactivateAll.
type Report struct {
	Files            []FileResult
	EnvRemoved       bool
	EnvErr           error
	AutostartRemoved bool
	AutostartErr     error
	DaemonStopped    bool
	KilledOrphans    int
	KilledProcesses  int
}

// Err returns an aggregated error if any cleanup step failed.
func (r Report) Err() error {
	var errs []error
	for _, f := range r.Files {
		if f.Err != nil {
			errs = append(errs, f.Err)
		}
	}
	if r.EnvErr != nil {
		errs = append(errs, r.EnvErr)
	}
	if r.AutostartErr != nil {
		errs = append(errs, r.AutostartErr)
	}
	return errors.Join(errs...)
}

// CleanAll discovers installations, restores patched files/archives, rolls back Desktop RU packs,
// cleans temporary artifacts, and clears proxy environment settings.
func CleanAll() error {
	return CleanInstalls(discovery.FindInstalls()).Err()
}

// CleanInstalls delegates file restoration and artifact cleanup to patcher.RestoreInstalls,
// removes Desktop localization bundles, and clears bypass proxy environment configuration.
func CleanInstalls(installs []discovery.Install) Report {
	var report Report

	resReport := patcher.RestoreInstalls(installs, patcher.RestoreOptions{CleanArtifacts: true})
	for _, f := range resReport.Files {
		report.Files = append(report.Files, FileResult{
			Path:     f.Path,
			Restored: f.Restored,
			Err:      f.Err,
		})
	}

	for _, inst := range installs {
		if inst.ResourcesDir == "" {
			continue
		}
		ruSt := rupack.InspectDesktop(inst)
		if !ruSt.HasBackup && !ruSt.BundleExists {
			continue
		}
		uRes, ruErr := rupack.Uninstall(inst, nil)
		changed := uRes != nil && (uRes.AsarRestored || uRes.BundleRemoved)
		if ruErr != nil || changed {
			report.Files = append(report.Files, FileResult{
				Path:     filepath.Join(inst.ResourcesDir, rupack.BundleDirName),
				Restored: changed,
				Err:      ruErr,
			})
		}
	}

	if err := env.RemoveProxyVar(); err != nil {
		report.EnvErr = err
	} else {
		report.EnvRemoved = true
	}

	return report
}

// DeactivateAll executes a full one-click system deactivation: closes running Antigravity processes,
// sweeps orphaned Sidecar SDK helpers, and executes independent rollback stages in parallel:
// restores client binaries and RU bundles, clears bypass proxy variables, stops the daemon,
// and removes autostart tasks.
func DeactivateAll(installs []discovery.Install) Report {
	var report Report

	// Stage 1: Close running apps and sweep orphaned Sidecar helpers
	report.KilledProcesses = CloseAntigravityProcesses()
	report.KilledOrphans = CleanOrphanAntigravityProcesses()

	var wg sync.WaitGroup
	var mu sync.Mutex

	// Stage 2A (parallel): Restore client files, asar and RU bundles
	wg.Add(1)
	go func() {
		defer wg.Done()
		resReport := patcher.RestoreInstalls(installs, patcher.RestoreOptions{CleanArtifacts: true})
		mu.Lock()
		for _, f := range resReport.Files {
			report.Files = append(report.Files, FileResult{
				Path:     f.Path,
				Restored: f.Restored,
				Err:      f.Err,
			})
		}
		mu.Unlock()

		for _, inst := range installs {
			if inst.ResourcesDir == "" {
				continue
			}
			ruSt := rupack.InspectDesktop(inst)
			if !ruSt.HasBackup && !ruSt.BundleExists {
				continue
			}
			uRes, ruErr := rupack.Uninstall(inst, nil)
			changed := uRes != nil && (uRes.AsarRestored || uRes.BundleRemoved)
			if ruErr != nil || changed {
				mu.Lock()
				report.Files = append(report.Files, FileResult{
					Path:     filepath.Join(inst.ResourcesDir, rupack.BundleDirName),
					Restored: changed,
					Err:      ruErr,
				})
				mu.Unlock()
			}
		}
	}()

	// Stage 2B (parallel): Remove proxy environment configuration
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := env.RemoveProxyVar(); err != nil {
			mu.Lock()
			report.EnvErr = err
			mu.Unlock()
		} else {
			mu.Lock()
			report.EnvRemoved = true
			mu.Unlock()
		}
	}()

	// Stage 2C (parallel): Stop background daemon and remove autostart tasks
	wg.Add(1)
	go func() {
		defer wg.Done()
		if cl, _, err := daemon.Connect(); err == nil {
			_ = cl.Shutdown()
			mu.Lock()
			report.DaemonStopped = true
			mu.Unlock()
		}
		if en, _ := autostart.IsEnabled(); en {
			if err := autostart.Disable(); err != nil {
				mu.Lock()
				report.AutostartErr = err
				mu.Unlock()
			} else {
				mu.Lock()
				report.AutostartRemoved = true
				mu.Unlock()
			}
		}
	}()

	wg.Wait()
	return report
}
