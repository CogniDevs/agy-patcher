package patcher

import (
	"errors"
	"os"
	"path/filepath"

	"agy-patcher/internal/hostenv"
	"agy-patcher/pkg/asar"
	"agy-patcher/pkg/discovery"
)

// EventLevel classifies log callbacks emitted by the orchestration engine.
type EventLevel string

const (
	LevelInfo EventLevel = "info"
	LevelOK   EventLevel = "ok"
	LevelWarn EventLevel = "warn"
	LevelErr  EventLevel = "err"
)

// EngineEventCode identifies an engine milestone so CLI/TUI/cleaner can format localized output uniformly.
type EngineEventCode string

const (
	EvInstallHeader        EngineEventCode = "install_header"
	EvInstallNoPatchNeeded EngineEventCode = "install_no_patch_needed"
	EvBinaryStart          EngineEventCode = "binary_start"
	EvBinarySkipped        EngineEventCode = "binary_skipped"
	EvBinaryPatched        EngineEventCode = "binary_patched"
	EvBinaryAlready        EngineEventCode = "binary_already"
	EvBinaryNoMatch        EngineEventCode = "binary_noop"
	EvBinaryProxyMissing   EngineEventCode = "binary_proxy_missing"
	EvBinaryError          EngineEventCode = "binary_err"
	EvAsarModern           EngineEventCode = "asar_modern"
	EvAsarExtracting       EngineEventCode = "asar_extracting"
	EvAsarRestored         EngineEventCode = "asar_restored"
	EvAsarError            EngineEventCode = "asar_err"
	EvJSStart              EngineEventCode = "js_start"
	EvJSPatched            EngineEventCode = "js_patched"
	EvJSAlready            EngineEventCode = "js_already"
	EvJSNoMatch            EngineEventCode = "js_noop"
	EvJSError              EngineEventCode = "js_err"
	EvRestoreFileOK        EngineEventCode = "restore_ok"
	EvRestoreFileOriginal  EngineEventCode = "restore_original"
	EvRestoreFileErr       EngineEventCode = "restore_err"
)

// EngineEvent describes a single structured event emitted during ApplyInstalls or RestoreInstalls.
type EngineEvent struct {
	Code    EngineEventCode
	Level   EventLevel
	Install discovery.Install
	Path    string
	Err     error
}

// FileOutcome summarizes restore/clean action on a single target path.
type FileOutcome struct {
	Path     string
	Restored bool
	Err      error
}

// EngineReport summarizes an orchestration run across a set of installations.
type EngineReport struct {
	Files         []FileOutcome
	HasErrors     bool
	NeedsSudoHint bool
}

// Err joins all per-file errors observed during the run.
func (r EngineReport) Err() error {
	var errs []error
	for _, f := range r.Files {
		if f.Err != nil {
			errs = append(errs, f.Err)
		}
	}
	return errors.Join(errs...)
}

// RestoreOptions configures RestoreInstalls.
type RestoreOptions struct {
	CleanArtifacts bool
	OnEvent        func(EngineEvent)
}

// ApplyInstalls executes binary, app.asar, JS, and extension patching across deduplicated targets.
func ApplyInstalls(installs []discovery.Install, onEvent func(EngineEvent)) EngineReport {
	var report EngineReport
	emit := func(ev EngineEvent) {
		if ev.Err != nil {
			report.HasErrors = true
			if errors.Is(ev.Err, os.ErrPermission) {
				report.NeedsSudoHint = true
			}
			report.Files = append(report.Files, FileOutcome{Path: ev.Path, Err: ev.Err})
		}
		if onEvent != nil {
			onEvent(ev)
		}
	}

	seenBin := make(map[string]bool)
	seenJS := make(map[string]bool)

	for _, inst := range installs {
		emit(EngineEvent{Code: EvInstallHeader, Level: LevelInfo, Install: inst, Path: inst.Path})

		bins := inst.Binaries
		if len(bins) == 0 && inst.BinaryPath != "" {
			bins = []string{inst.BinaryPath}
		}
		if inst.Type == discovery.TypeJetBrains && len(bins) == 0 && inst.JsPath == "" && (inst.ExtensionJsPath == "" || !NeedsExtensionJSPatch(inst.ExtensionJsPath)) {
			emit(EngineEvent{Code: EvInstallNoPatchNeeded, Level: LevelInfo, Install: inst, Path: inst.Path})
			continue
		}
		for _, bin := range bins {
			if bin == "" {
				continue
			}
			key := hostenv.NormalizePathKey(bin)
			if seenBin[key] {
				emit(EngineEvent{Code: EvBinarySkipped, Level: LevelInfo, Install: inst, Path: bin})
				continue
			}
			seenBin[key] = true

			emit(EngineEvent{Code: EvBinaryStart, Level: LevelInfo, Install: inst, Path: bin})
			res, err := ApplyBinaryPatchDetailed(bin)
			if err != nil {
				emit(EngineEvent{Code: EvBinaryError, Level: LevelErr, Install: inst, Path: bin, Err: err})
				continue
			}

			switch res.FieldOutcome {
			case OutcomePatched:
				emit(EngineEvent{Code: EvBinaryPatched, Level: LevelOK, Install: inst, Path: bin})
			case OutcomeAlreadyPatched:
				emit(EngineEvent{Code: EvBinaryAlready, Level: LevelInfo, Install: inst, Path: bin})
			case OutcomeNoMatch:
				if inst.Type == discovery.TypeJetBrains {
					emit(EngineEvent{Code: EvInstallNoPatchNeeded, Level: LevelInfo, Install: inst, Path: bin})
					continue
				}
				report.HasErrors = true
				emit(EngineEvent{Code: EvBinaryNoMatch, Level: LevelWarn, Install: inst, Path: bin})
			}

			if res.ProxyVarOutcome == OutcomeNoMatch && res.FieldOutcome != OutcomeNoMatch {
				emit(EngineEvent{Code: EvBinaryProxyMissing, Level: LevelWarn, Install: inst, Path: bin})
			}
		}

		skipDesktopJS := false
		if inst.AsarPath != "" {
			if src, err := asar.ReadEntry(inst.AsarPath, "dist/main.js"); err == nil && IsNewDesktopArchitecture(string(src)) {
				emit(EngineEvent{Code: EvAsarModern, Level: LevelInfo, Install: inst, Path: inst.AsarPath})
				if _, err := asar.RestorePristine(inst.ResourcesDir); err != nil {
					emit(EngineEvent{Code: EvAsarError, Level: LevelErr, Install: inst, Path: inst.AsarPath, Err: err})
				}
				skipDesktopJS = true
			} else if _, statErr := os.Stat(inst.AsarPath); statErr == nil {
				emit(EngineEvent{Code: EvAsarExtracting, Level: LevelInfo, Install: inst, Path: inst.AsarPath})
				appDir := filepath.Join(inst.ResourcesDir, "app")
				if err := asar.Extract(inst.AsarPath, appDir); err != nil {
					emit(EngineEvent{Code: EvAsarError, Level: LevelErr, Install: inst, Path: inst.AsarPath, Err: err})
				} else if inst.JsPath == "" {
					distMain := filepath.Join(appDir, "dist", "main.js")
					if hostenv.IsRegularFile(distMain) {
						inst.JsPath = distMain
					}
				}
			}
		}

		if !skipDesktopJS && inst.JsPath != "" {
			key := hostenv.NormalizePathKey(inst.JsPath)
			if !seenJS[key] {
				seenJS[key] = true
				emit(EngineEvent{Code: EvJSStart, Level: LevelInfo, Install: inst, Path: inst.JsPath})
				out, err := ApplyJSPatch(inst.JsPath)
				if err != nil {
					emit(EngineEvent{Code: EvJSError, Level: LevelErr, Install: inst, Path: inst.JsPath, Err: err})
				} else {
					switch out {
					case OutcomePatched:
						emit(EngineEvent{Code: EvJSPatched, Level: LevelOK, Install: inst, Path: inst.JsPath})
					case OutcomeAlreadyPatched:
						emit(EngineEvent{Code: EvJSAlready, Level: LevelInfo, Install: inst, Path: inst.JsPath})
					case OutcomeNoMatch:
						emit(EngineEvent{Code: EvJSNoMatch, Level: LevelInfo, Install: inst, Path: inst.JsPath})
						if inst.Type == discovery.TypeDesktop && inst.ResourcesDir != "" {
							_, _ = asar.RestorePristine(inst.ResourcesDir)
						}
					}
				}
			}
		}

		if inst.ExtensionJsPath != "" {
			key := hostenv.NormalizePathKey(inst.ExtensionJsPath)
			if !seenJS[key] && NeedsExtensionJSPatch(inst.ExtensionJsPath) {
				seenJS[key] = true
				out, err := PatchExtensionJS(inst.ExtensionJsPath)
				if err != nil {
					emit(EngineEvent{Code: EvJSError, Level: LevelWarn, Install: inst, Path: inst.ExtensionJsPath, Err: err})
				} else if out == OutcomePatched {
					emit(EngineEvent{Code: EvJSPatched, Level: LevelOK, Install: inst, Path: inst.ExtensionJsPath})
				}
			}
		}
	}
	return report
}

// RestoreInstalls reverts our patched binaries, JS bundles, and app.asar archives across installs.
func RestoreInstalls(installs []discovery.Install, opts RestoreOptions) EngineReport {
	var report EngineReport
	emit := func(ev EngineEvent) {
		if opts.OnEvent != nil {
			opts.OnEvent(ev)
		}
	}

	seenFiles := make(map[string]bool)
	for _, inst := range installs {
		for _, target := range inst.TargetPaths() {
			key := hostenv.NormalizePathKey(target)
			if seenFiles[key] {
				continue
			}
			seenFiles[key] = true

			outcome := FileOutcome{Path: target}
			if opts.CleanArtifacts {
				if IsPatched(target) || HasBackup(target) {
					err := RestoreFile(target)
					if err == nil {
						outcome.Restored = true
					} else if !errors.Is(err, ErrNoBackup) {
						outcome.Err = err
					}
				}
				if err := CleanArtifacts(target); err != nil && outcome.Err == nil {
					outcome.Err = err
				}
			} else {
				err := RestoreFile(target)
				switch {
				case err == nil:
					outcome.Restored = true
					emit(EngineEvent{Code: EvRestoreFileOK, Level: LevelOK, Install: inst, Path: target})
				case errors.Is(err, ErrNoBackup) && !IsPatched(target):
					emit(EngineEvent{Code: EvRestoreFileOriginal, Level: LevelInfo, Install: inst, Path: target})
				default:
					outcome.Err = err
					emit(EngineEvent{Code: EvRestoreFileErr, Level: LevelErr, Install: inst, Path: target, Err: err})
				}
			}

			if outcome.Err != nil {
				report.HasErrors = true
				if errors.Is(outcome.Err, os.ErrPermission) {
					report.NeedsSudoHint = true
				}
			}
			report.Files = append(report.Files, outcome)
		}

		if inst.ResourcesDir != "" {
			asarPath := filepath.Join(inst.ResourcesDir, "app.asar")
			restored, err := asar.RestorePristine(inst.ResourcesDir)
			if err != nil {
				report.HasErrors = true
				if errors.Is(err, os.ErrPermission) {
					report.NeedsSudoHint = true
				}
				report.Files = append(report.Files, FileOutcome{Path: asarPath, Err: err})
				emit(EngineEvent{Code: EvAsarError, Level: LevelErr, Install: inst, Path: asarPath, Err: err})
			} else if restored {
				report.Files = append(report.Files, FileOutcome{Path: asarPath, Restored: true})
				emit(EngineEvent{Code: EvAsarRestored, Level: LevelOK, Install: inst, Path: asarPath})
			}
		}
	}
	return report
}
