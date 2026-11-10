//go:build windows

package hostenv

import (
	"fmt"
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
