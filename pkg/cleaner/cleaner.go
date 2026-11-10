package cleaner

import (
	"errors"
	"path/filepath"

	"agy-patcher/pkg/discovery"
	"agy-patcher/pkg/env"
	"agy-patcher/pkg/patcher"
	"agy-patcher/pkg/rupack"
)

// FileResult records the cleanup outcome for a single target file, archive, or localization bundle.
type FileResult struct {
	Path     string
	Restored bool
	Err      error
}

// Report describes all actions performed by CleanInstalls.
type Report struct {
	Files      []FileResult
	EnvRemoved bool
	EnvErr     error
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
