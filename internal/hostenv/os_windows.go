//go:build windows

package hostenv

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

const (
	winEnvSubkey         = "Environment"
	winMachineSubkey     = `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`
	keySetValue          = 0x0002
	keyQueryValue        = 0x0001
	regSZ                = 1
	regExpandSZ          = 2
	hwndBroadcast        = 0xffff
	wmSettingChange      = 0x001A
	smtoAbortIfHung      = 0x0002
	procQueryLimitedInfo = 0x1000
	procTerminateRight   = 0x0001
)

var (
	advapi32                   = syscall.NewLazyDLL("advapi32.dll")
	procRegCreateKeyExW        = advapi32.NewProc("RegCreateKeyExW")
	procRegOpenKeyExW          = advapi32.NewProc("RegOpenKeyExW")
	procRegQueryValueExW       = advapi32.NewProc("RegQueryValueExW")
	procRegSetValueExW         = advapi32.NewProc("RegSetValueExW")
	procRegDeleteValueW        = advapi32.NewProc("RegDeleteValueW")
	procRegCloseKey            = advapi32.NewProc("RegCloseKey")
	user32                     = syscall.NewLazyDLL("user32.dll")
	procSendMessageTimeoutW    = user32.NewProc("SendMessageTimeoutW")
	kernel32                   = syscall.NewLazyDLL("kernel32.dll")
	procQueryFullProcessImageW = kernel32.NewProc("QueryFullProcessImageNameW")
)

// MachineEnvSubkey is the HKLM system environment registry subkey.
const MachineEnvSubkey = winMachineSubkey

// UserEnvSubkey is the HKCU user environment registry subkey.
const UserEnvSubkey = winEnvSubkey

// ReadRegString reads a REG_SZ or REG_EXPAND_SZ value from root\subkey\name.
func ReadRegString(root syscall.Handle, subkey, name string) string {
	subkeyPtr, err := syscall.UTF16PtrFromString(subkey)
	if err != nil {
		return ""
	}
	var h syscall.Handle
	if err := syscall.RegOpenKeyEx(root, subkeyPtr, 0, syscall.KEY_QUERY_VALUE, &h); err != nil {
		return ""
	}
	defer syscall.RegCloseKey(h)
	return ReadOpenedRegString(h, name)
}

// ReadOpenedRegString reads a REG_SZ or REG_EXPAND_SZ value from an already open registry key handle.
func ReadOpenedRegString(h syscall.Handle, name string) string {
	namePtr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return ""
	}
	var valType, bufLen uint32
	if err := syscall.RegQueryValueEx(h, namePtr, nil, &valType, nil, &bufLen); err != nil || bufLen < 2 || (valType != regSZ && valType != regExpandSZ) {
		return ""
	}
	buf := make([]byte, bufLen)
	if err := syscall.RegQueryValueEx(h, namePtr, nil, &valType, &buf[0], &bufLen); err != nil || bufLen < 2 {
		return ""
	}
	u16 := unsafe.Slice((*uint16)(unsafe.Pointer(&buf[0])), int(bufLen)/2)
	return strings.TrimSpace(syscall.UTF16ToString(u16))
}

// WriteRegString creates/opens root\subkey and sets name to a REG_SZ value.
func WriteRegString(root syscall.Handle, subkey, name, value string) error {
	subkeyPtr, err := syscall.UTF16PtrFromString(subkey)
	if err != nil {
		return err
	}
	var h syscall.Handle
	r1, _, callErr := procRegCreateKeyExW.Call(
		uintptr(root),
		uintptr(unsafe.Pointer(subkeyPtr)),
		0, 0, 0,
		keySetValue|keyQueryValue,
		0,
		uintptr(unsafe.Pointer(&h)),
		0,
	)
	if r1 != 0 {
		return regErr("RegCreateKeyExW", name, r1, callErr)
	}
	defer procRegCloseKey.Call(uintptr(h))

	namePtr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	u16Val, err := syscall.UTF16FromString(value)
	if err != nil {
		return err
	}

	r1, _, callErr = procRegSetValueExW.Call(
		uintptr(h),
		uintptr(unsafe.Pointer(namePtr)),
		0,
		uintptr(regSZ),
		uintptr(unsafe.Pointer(&u16Val[0])),
		uintptr(len(u16Val)*2),
	)
	if r1 != 0 {
		return regErr("RegSetValueExW", name, r1, callErr)
	}
	return nil
}

// DeleteRegValue deletes name under root\subkey, returning (false, nil) if absent.
func DeleteRegValue(root syscall.Handle, subkey, name string) (bool, error) {
	subkeyPtr, err := syscall.UTF16PtrFromString(subkey)
	if err != nil {
		return false, err
	}
	var h syscall.Handle
	r1, _, _ := procRegOpenKeyExW.Call(
		uintptr(root),
		uintptr(unsafe.Pointer(subkeyPtr)),
		0,
		keySetValue,
		uintptr(unsafe.Pointer(&h)),
	)
	if r1 != 0 {
		if r1 == uintptr(syscall.ERROR_FILE_NOT_FOUND) {
			return false, nil
		}
		return false, fmt.Errorf("open registry %s: errno %d", subkey, r1)
	}
	defer procRegCloseKey.Call(uintptr(h))

	namePtr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return false, err
	}
	r1, _, callErr := procRegDeleteValueW.Call(uintptr(h), uintptr(unsafe.Pointer(namePtr)))
	if r1 == uintptr(syscall.ERROR_FILE_NOT_FOUND) {
		return false, nil
	}
	if r1 != 0 {
		return false, regErr("RegDeleteValueW", name, r1, callErr)
	}
	return true, nil
}

// ReadUserEnv returns a HKCU\Environment value.
func ReadUserEnv(name string) string {
	return ReadRegString(syscall.HKEY_CURRENT_USER, winEnvSubkey, name)
}

// WriteUserEnv sets HKCU\Environment\name and broadcasts WM_SETTINGCHANGE.
func WriteUserEnv(name, value string) error {
	if err := WriteRegString(syscall.HKEY_CURRENT_USER, winEnvSubkey, name, value); err != nil {
		return err
	}
	BroadcastEnvChange()
	return nil
}

// DeleteUserEnv removes HKCU\Environment\name and broadcasts WM_SETTINGCHANGE if removed.
func DeleteUserEnv(name string) (bool, error) {
	removed, err := DeleteRegValue(syscall.HKEY_CURRENT_USER, winEnvSubkey, name)
	if err == nil && removed {
		BroadcastEnvChange()
	}
	return removed, err
}

// BroadcastEnvChange notifies top-level Windows processes of Environment changes.
func BroadcastEnvChange() {
	if envPtr, err := syscall.UTF16PtrFromString("Environment"); err == nil {
		var result uintptr
		_, _, _ = procSendMessageTimeoutW.Call(
			hwndBroadcast,
			wmSettingChange,
			0,
			uintptr(unsafe.Pointer(envPtr)),
			smtoAbortIfHung,
			1000,
			uintptr(unsafe.Pointer(&result)),
		)
	}
}

// EnumUninstallDirs scans HKLM/HKCU Uninstall keys using matchFn(displayName, installLocation).
func EnumUninstallDirs(matchFn func(displayName, installLocation string) bool) []string {
	type regRoot struct {
		root syscall.Handle
		path string
	}
	roots := []regRoot{
		{syscall.HKEY_LOCAL_MACHINE, `SOFTWARE\Wow6432Node\Microsoft\Windows\CurrentVersion\Uninstall`},
		{syscall.HKEY_LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`},
		{syscall.HKEY_CURRENT_USER, `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`},
	}
	var dirs []string
	for _, r := range roots {
		subkeyPtr, err := syscall.UTF16PtrFromString(r.path)
		if err != nil {
			continue
		}
		var h syscall.Handle
		if err := syscall.RegOpenKeyEx(r.root, subkeyPtr, 0, syscall.KEY_ENUMERATE_SUB_KEYS|syscall.KEY_QUERY_VALUE, &h); err != nil {
			continue
		}
		nameBuf := make([]uint16, 512)
		for idx := uint32(0); ; idx++ {
			nameLen := uint32(len(nameBuf))
			if err := syscall.RegEnumKeyEx(h, idx, &nameBuf[0], &nameLen, nil, nil, nil, nil); err != nil {
				break
			}
			if int(nameLen) < len(nameBuf) {
				nameBuf[nameLen] = 0
			} else {
				nameBuf[len(nameBuf)-1] = 0
			}
			if nameLen == 0 {
				continue
			}
			var childH syscall.Handle
			if err := syscall.RegOpenKeyEx(h, &nameBuf[0], 0, syscall.KEY_QUERY_VALUE, &childH); err != nil {
				continue
			}
			disp := ReadOpenedRegString(childH, "DisplayName")
			loc := strings.Trim(strings.TrimSpace(ReadOpenedRegString(childH, "InstallLocation")), `"'`)
			_ = syscall.RegCloseKey(childH)
			if loc != "" && matchFn(disp, loc) {
				dirs = append(dirs, loc)
			}
		}
		_ = syscall.RegCloseKey(h)
	}
	return dirs
}

// TerminateProcessByBinary terminates only processes whose full executable image path matches binPath.
func TerminateProcessByBinary(binPath string) {
	absTarget, err := filepath.Abs(binPath)
	if err != nil {
		absTarget = filepath.Clean(binPath)
	}
	if absTarget == "" {
		return
	}
	targetName := filepath.Base(absTarget)

	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return
	}
	defer syscall.CloseHandle(snap)

	var pe syscall.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	if err := syscall.Process32First(snap, &pe); err != nil {
		return
	}

	pathBuf := make([]uint16, 1024)
	for {
		exeName := syscall.UTF16ToString(pe.ExeFile[:])
		if strings.EqualFold(exeName, targetName) && pe.ProcessID != 0 {
			h, openErr := syscall.OpenProcess(procQueryLimitedInfo|procTerminateRight, false, pe.ProcessID)
			if openErr == nil {
				n := uint32(len(pathBuf))
				r1, _, _ := procQueryFullProcessImageW.Call(
					uintptr(h),
					0,
					uintptr(unsafe.Pointer(&pathBuf[0])),
					uintptr(unsafe.Pointer(&n)),
				)
				if r1 != 0 && n > 0 {
					imgPath := filepath.Clean(syscall.UTF16ToString(pathBuf[:n]))
					if strings.EqualFold(imgPath, absTarget) {
						_ = syscall.TerminateProcess(h, 1)
					}
				}
				_ = syscall.CloseHandle(h)
			}
		}
		if err := syscall.Process32Next(snap, &pe); err != nil {
			break
		}
	}
}

func regErr(op, name string, r1 uintptr, callErr error) error {
	if callErr != nil && callErr != syscall.Errno(0) {
		return fmt.Errorf("%s(%s): %w", op, name, callErr)
	}
	return fmt.Errorf("%s(%s): errno %d", op, name, r1)
}

// isSelfOrAncestor checks if targetPID is the current process or one of its parent processes.
func isSelfOrAncestor(targetPID uint32, activeParents map[uint32]uint32) bool {
	selfPID := uint32(syscall.Getpid())
	if targetPID == 0 || targetPID == selfPID {
		return true
	}
	curr := selfPID
	for i := 0; i < 32; i++ {
		parent, ok := activeParents[curr]
		if !ok || parent == 0 {
			break
		}
		if parent == targetPID {
			return true
		}
		curr = parent
	}
	return false
}

// TerminateOrphanProcesses scans active processes and terminates those matching targetNames
// whose parent process no longer exists in the system (orphaned Sidecar SDK helpers).
// Processes whose parent is alive are preserved.
func TerminateOrphanProcesses(targetNames []string) int {
	if len(targetNames) == 0 {
		return 0
	}
	targetMap := make(map[string]bool, len(targetNames))
	for _, n := range targetNames {
		targetMap[strings.ToLower(strings.TrimSpace(n))] = true
	}

	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0
	}
	defer syscall.CloseHandle(snap)

	var pe syscall.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	if err := syscall.Process32First(snap, &pe); err != nil {
		return 0
	}

	type candidate struct {
		pid       uint32
		parentPID uint32
	}

	activePIDs := make(map[uint32]bool)
	parentMap := make(map[uint32]uint32)
	var candidates []candidate

	for {
		exeName := syscall.UTF16ToString(pe.ExeFile[:])
		pid := pe.ProcessID
		parentPID := pe.ParentProcessID
		if pid != 0 {
			activePIDs[pid] = true
			parentMap[pid] = parentPID
			if targetMap[strings.ToLower(exeName)] {
				candidates = append(candidates, candidate{
					pid:       pid,
					parentPID: parentPID,
				})
			}
		}
		if err := syscall.Process32Next(snap, &pe); err != nil {
			break
		}
	}

	killed := 0
	for _, c := range candidates {
		// Never touch ourselves or our ancestor processes.
		if isSelfOrAncestor(c.pid, parentMap) {
			continue
		}
		// If the parent process no longer exists in the snapshot, the process is orphaned.
		if c.parentPID == 0 || !activePIDs[c.parentPID] {
			h, err := syscall.OpenProcess(procTerminateRight, false, c.pid)
			if err == nil {
				if err := syscall.TerminateProcess(h, 1); err == nil {
					killed++
				}
				_ = syscall.CloseHandle(h)
			}
		}
	}
	return killed
}

// CloseProcessesByNames forcefully terminates all processes matching any of the given executable names.
func CloseProcessesByNames(names []string) int {
	if len(names) == 0 {
		return 0
	}
	nameMap := make(map[string]bool, len(names))
	for _, n := range names {
		nameMap[strings.ToLower(strings.TrimSpace(n))] = true
	}

	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0
	}
	defer syscall.CloseHandle(snap)

	var pe syscall.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	if err := syscall.Process32First(snap, &pe); err != nil {
		return 0
	}

	parentMap := make(map[uint32]uint32)
	type hit struct {
		pid uint32
	}
	var hits []hit

	for {
		exeName := syscall.UTF16ToString(pe.ExeFile[:])
		if pe.ProcessID != 0 {
			parentMap[pe.ProcessID] = pe.ParentProcessID
			if nameMap[strings.ToLower(exeName)] {
				hits = append(hits, hit{pid: pe.ProcessID})
			}
		}
		if err := syscall.Process32Next(snap, &pe); err != nil {
			break
		}
	}

	killed := 0
	for _, hProc := range hits {
		if isSelfOrAncestor(hProc.pid, parentMap) {
			continue
		}
		h, err := syscall.OpenProcess(procTerminateRight, false, hProc.pid)
		if err == nil {
			if err := syscall.TerminateProcess(h, 1); err == nil {
				killed++
			}
			_ = syscall.CloseHandle(h)
		}
	}
	return killed
}

var procProcessIdToSessionId = kernel32.NewProc("ProcessIdToSessionId")
var procGetCurrentProcessId = kernel32.NewProc("GetCurrentProcessId")

// InServiceSession reports whether the current process runs in Windows Session 0 (service/S4U task).
func InServiceSession() bool {
	pid, _, _ := procGetCurrentProcessId.Call()
	var sid uint32 = 0xFFFFFFFF
	r1, _, _ := procProcessIdToSessionId.Call(pid, uintptr(unsafe.Pointer(&sid)))
	return r1 != 0 && sid == 0
}

// LaunchInUserSession spawns app in the active user interactive session using a temporary scheduled task.
// This prevents Session 0 S4U tasks from starting invisible UI processes that hold Electron single-instance locks.
func LaunchInUserSession(app string) error {
	absApp, err := filepath.Abs(app)
	if err != nil {
		absApp = app
	}
	dir := filepath.Dir(absApp)
	stem := strings.TrimSuffix(filepath.Base(absApp), filepath.Ext(absApp))
	script := fmt.Sprintf(`$ErrorActionPreference='Stop'; $n='AG Unlocker Reopen'; `+
		`$a=New-ScheduledTaskAction -Execute '%s' -WorkingDirectory '%s'; `+
		`$p=New-ScheduledTaskPrincipal -UserId "$env:USERDOMAIN\$env:USERNAME" -LogonType Interactive -RunLevel Limited; `+
		`$s=New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -MultipleInstances IgnoreNew -ExecutionTimeLimit ([TimeSpan]::Zero); `+
		`Register-ScheduledTask -TaskName $n -Action $a -Principal $p -Settings $s -Description 'Antigravity Unlocker: reopen the app in the user session' -Force | Out-Null; `+
		`Start-ScheduledTask -TaskName $n; `+
		`for ($i=0; $i -lt 20; $i++) { if (Get-Process -Name '%s' -ErrorAction SilentlyContinue | Where-Object { $_.SessionId -ne 0 }) { exit 0 }; Start-Sleep -Milliseconds 500 }; `+
		`[Console]::Error.WriteLine('process not seen in a user session'); exit 1`,
		strings.ReplaceAll(absApp, "'", "''"),
		strings.ReplaceAll(dir, "'", "''"),
		strings.ReplaceAll(stem, "'", "''"),
	)
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("launch in user session failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}
