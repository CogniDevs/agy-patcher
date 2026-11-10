package patcher

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"agy-patcher/internal/hostenv"
)

const (
	ideFooterTag = "// [AGY_CORE_PATCHED]"
	extHeaderTag = "/*[AGY_EXT_PATCHED]*/"
)

var (
	reIDEAuthFlow = regexp.MustCompile(
		`async\s+([A-Za-z_$0-9]+)\(([A-Za-z_$0-9]+)\)\s*\{\s*if\(this\.([A-Za-z_$0-9]+)\.send\(\{type:[A-Za-z_$0-9]+\.isGcpTos\?"GCP_SIGN_IN":"SIGN_IN"\}\),this\.([A-Za-z_$0-9]+)\.resetIsTierGCPTos\(\),this\.[A-Za-z_$0-9]+\.isGoogleInternal\)\{try\{await this\.([A-Za-z_$0-9]+)\.loadCodeAssist\([A-Za-z_$0-9]+\);const\{settings:([A-Za-z_$0-9]+),userTier:([A-Za-z_$0-9]+)\}=await this\.refreshUserStatus\([A-Za-z_$0-9]+\),([A-Za-z_$0-9]+)=([A-Za-z_$0-9]+)\([A-Za-z_$0-9]+\);this\.([A-Za-z_$0-9]+)\.pushUpdate\([A-Za-z_$0-9]+\),this\.[A-Za-z_$0-9]+\.send\(\{type:"AUTH_SUCCESS",tokenInfo:[A-Za-z_$0-9]+\}\),this\.([A-Za-z_$0-9]+)\.fire\(\{settings:[A-Za-z_$0-9]+,userTier:[A-Za-z_$0-9]+\}\)\}catch\(([A-Za-z_$0-9]+)\)\{.*?(?:return\}|return;\s*\})`,
	)

	reDesktopUserStatusCheck = regexp.MustCompile(
		`let ([A-Za-z_$]+)=.*\.getUserStatus\(\{\}\)\)\)\.userStatus;if\(([A-Za-z_$]+)\)\{`,
	)
	reDesktopErrorHandler = regexp.MustCompile(
		`_handleAuthErrorResponse\(([A-Za-z_$]+)\)\{var ([A-Za-z_$]+)=([A-Za-z_$]+)\?\.failureDetails;`,
	)
	reDesktopIneligibleCase = regexp.MustCompile(
		`\?\.failureDetails\?\.case==="ineligible"\?this\._authActor\.send\(\{type:"SET_INELIGIBLE"`,
	)
	reDesktopEligibleFlag = regexp.MustCompile(`(isEligible:\s*)false`)

	reExtSidebarStatus = regexp.MustCompile(
		`const t=await ([A-Za-z_$][A-Za-z_$0-9.]*)\.UserStatus\.getUserStatus\(\);if\(!t\)return\[\];const n=\(0,([A-Za-z_$][A-Za-z_$0-9.]*)\)\(t,([A-Za-z_$][A-Za-z_$0-9.]*)\),\{email:([A-Za-z_$][A-Za-z_$0-9]*),name:([A-Za-z_$][A-Za-z_$0-9]*)\}=n;return""===([A-Za-z_$][A-Za-z_$0-9]*)\?\[\]:`,
	)
)

// IsNewDesktopArchitecture checks whether dist/main.js belongs to modern modular Desktop builds (v2.4+)
// where authentication is handled inside the native Language Server executable.
func IsNewDesktopArchitecture(content string) bool {
	isModular := strings.Contains(content, "./languageServer") && strings.Contains(content, "./ipcHandlers")
	hasInlineAuth := strings.Contains(content, "_handleAuthErrorResponse") ||
		strings.Contains(content, "getUserStatus") ||
		strings.Contains(content, `"AUTH_SUCCESS"`) ||
		strings.Contains(content, `"SET_INELIGIBLE"`)
	return isModular && !hasInlineAuth
}

// PatchIDEJS patches IDE main.js to bypass the isGoogleInternal authentication gate cleanly.
func PatchIDEJS(mainJSPath string) (PatchOutcome, error) {
	raw, err := os.ReadFile(mainJSPath)
	if err != nil {
		return OutcomeNoMatch, err
	}
	content := string(raw)

	if hasFooterTag(content) {
		return OutcomeAlreadyPatched, nil
	}

	loc := reIDEAuthFlow.FindStringSubmatchIndex(content)
	if loc == nil {
		return OutcomeNoMatch, nil
	}
	capture := func(groupIdx int) string {
		return content[loc[groupIdx*2]:loc[groupIdx*2+1]]
	}

	methodName := capture(1)
	authArg := capture(2)
	busProp := capture(3)
	clientProp := capture(4)
	tokenVar := capture(8)
	tokenMapper := capture(9)
	syncProp := capture(10)
	emitterProp := capture(11)

	replacement := fmt.Sprintf(
		`async %s(%s){this.%s.send({type:%s.isGcpTos?"GCP_SIGN_IN":"SIGN_IN"});this.%s.resetIsTierGCPTos();let __agyState={settings:{},userTier:{id:"pro",description:"Pro"}};try{await this.%s.loadCodeAssist(%s).catch(()=>{});await this.%s.onboardUser("standard-tier",%s).catch(()=>this.%s.onboardUser("free-tier",%s)).catch(()=>{});try{__agyState=await this.refreshUserStatus(%s);}catch(_){};const %s=%s(%s);try{this.%s.pushUpdate(%s);}catch(_){};this.%s.send({type:"AUTH_SUCCESS",tokenInfo:%s});this.%s.fire({settings:__agyState.settings,userTier:__agyState.userTier});}catch(_){}return;`,
		methodName, authArg,
		busProp, authArg,
		clientProp,
		clientProp, authArg,
		clientProp, authArg, clientProp, authArg,
		authArg,
		tokenVar, tokenMapper, authArg,
		syncProp, tokenVar,
		busProp, authArg,
		emitterProp,
	)

	updated := content[:loc[0]] + replacement + content[loc[1]:] + "\n" + ideFooterTag
	if err := saveJSBackupOnce(mainJSPath, raw); err != nil {
		return OutcomeNoMatch, err
	}
	if err := hostenv.WriteFileAtomic(mainJSPath, []byte(updated), hostenv.FileModeOr(mainJSPath, 0644), nil); err != nil {
		return OutcomeNoMatch, err
	}
	return OutcomePatched, nil
}

// PatchDesktopJS patches older Electron Desktop bundles and leaves modular v2.4+ bundles untouched.
func PatchDesktopJS(distMainJSPath string) (PatchOutcome, error) {
	raw, err := os.ReadFile(distMainJSPath)
	if err != nil {
		return OutcomeNoMatch, err
	}
	content := string(raw)

	if hasFooterTag(content) {
		return OutcomeAlreadyPatched, nil
	}
	if IsNewDesktopArchitecture(content) {
		return OutcomeNoMatch, nil
	}

	rewritten := patchShellDesktopContent(content)
	if rewritten == content {
		return OutcomeNoMatch, nil
	}

	if err := saveJSBackupOnce(distMainJSPath, raw); err != nil {
		return OutcomeNoMatch, err
	}
	if err := hostenv.WriteFileAtomic(distMainJSPath, []byte(rewritten+"\n"+ideFooterTag), hostenv.FileModeOr(distMainJSPath, 0644), nil); err != nil {
		return OutcomeNoMatch, err
	}
	return OutcomePatched, nil
}

// PatchExtensionJS patches extension.js in older editor extensions so empty emails do not hide UI panels.
func PatchExtensionJS(extJSPath string) (PatchOutcome, error) {
	raw, err := os.ReadFile(extJSPath)
	if err != nil {
		return OutcomeNoMatch, err
	}
	content := string(raw)
	if strings.Contains(content, extHeaderTag) {
		return OutcomeAlreadyPatched, nil
	}

	loc := reExtSidebarStatus.FindStringSubmatchIndex(content)
	if loc == nil {
		return OutcomeNoMatch, nil
	}
	capture := func(groupIdx int) string {
		return content[loc[groupIdx*2]:loc[groupIdx*2+1]]
	}

	modNS := capture(1)
	decodeFn := capture(2)
	decodeArg := capture(3)
	mailVar := capture(4)
	nameVar := capture(5)

	rewrittenBlock := fmt.Sprintf(
		`const t=await %s.UserStatus.getUserStatus();let %s="antigravity-user",%s="User";if(t){try{const n=(0,%s)(t,%s);if(n?.email)%s=n.email;if(n?.name)%s=n.name;}catch(_){}}if(false)return[];`,
		modNS, mailVar, nameVar, decodeFn, decodeArg, mailVar, nameVar,
	)

	updated := extHeaderTag + "\n" + content[:loc[0]] + rewrittenBlock + content[loc[1]:]
	if err := saveJSBackupOnce(extJSPath, raw); err != nil {
		return OutcomeNoMatch, err
	}
	if err := hostenv.WriteFileAtomic(extJSPath, []byte(updated), hostenv.FileModeOr(extJSPath, 0644), nil); err != nil {
		return OutcomeNoMatch, err
	}
	return OutcomePatched, nil
}

// NeedsExtensionJSPatch reports whether extension.js carries the UserStatus check or our patch tag.
func NeedsExtensionJSPatch(extJSPath string) bool {
	if extJSPath == "" {
		return false
	}
	raw, err := os.ReadFile(extJSPath)
	if err != nil {
		return false
	}
	s := string(raw)
	return strings.Contains(s, extHeaderTag) ||
		hasFooterTag(s) ||
		reExtSidebarStatus.MatchString(s)
}

// ApplyJSPatch inspects a JS target path and applies the matching JS patch.
func ApplyJSPatch(path string) (PatchOutcome, error) {
	norm := strings.ToLower(filepath.ToSlash(path))
	switch {
	case strings.HasSuffix(norm, "/out/main.js"):
		out, err := PatchIDEJS(path)
		if err != nil || out != OutcomeNoMatch {
			return out, err
		}
		return applyInlineJSTokenFallback(path)
	case strings.HasSuffix(norm, "/dist/main.js"):
		return PatchDesktopJS(path)
	case strings.HasSuffix(norm, "/extension.js"):
		return PatchExtensionJS(path)
	default:
		return applyInlineJSTokenFallback(path)
	}
}

// PatchJS applies ApplyJSPatch and returns ErrNothingToPatch if no known signature matched.
func PatchJS(path string) error {
	out, err := ApplyJSPatch(path)
	if err != nil {
		return err
	}
	if out == OutcomeNoMatch {
		return ErrNothingToPatch
	}
	return nil
}

func applyInlineJSTokenFallback(path string) (PatchOutcome, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return OutcomeNoMatch, err
	}
	content := string(raw)
	if hasFooterTag(content) || strings.Contains(content, FieldToken) {
		return OutcomeAlreadyPatched, nil
	}
	if !strings.Contains(content, "ineligible") {
		return OutcomeNoMatch, nil
	}
	updated := strings.ReplaceAll(content, "ineligible", FieldToken)
	if err := saveJSBackupOnce(path, raw); err != nil {
		return OutcomeNoMatch, err
	}
	if err := hostenv.WriteFileAtomic(path, []byte(updated+"\n"+ideFooterTag), hostenv.FileModeOr(path, 0644), nil); err != nil {
		return OutcomeNoMatch, err
	}
	return OutcomePatched, nil
}

// RestoreJS restores a JS file from its .agy_orig backup created by agy-patcher.
func RestoreJS(path string) error {
	bak := path + primaryBackupExt
	if HasBackupFile(bak) {
		return restoreFromBackup(path, bak)
	}
	return fmt.Errorf("%w: %s", ErrNoBackup, bak)
}

// IsJSPatched reports whether a JS bundle holds agy-patcher markers or tokens.
func IsJSPatched(path string) bool {
	if path == "" {
		return false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	s := string(raw)
	return hasFooterTag(s) ||
		strings.Contains(s, extHeaderTag) ||
		strings.Contains(s, FieldToken)
}

func saveJSBackupOnce(target string, originalData []byte) error {
	bakPath := target + primaryBackupExt
	if HasBackupFile(bakPath) {
		return nil
	}
	if err := hostenv.WriteFileAtomic(bakPath, originalData, hostenv.FileModeOr(target, 0644), nil); err != nil {
		return fmt.Errorf("create backup %s: %w", bakPath, err)
	}
	return nil
}

func hasFooterTag(content string) bool {
	trimmed := strings.TrimRight(content, " \t\r\n")
	if trimmed == "" {
		return false
	}
	lastLine := trimmed
	if idx := strings.LastIndexByte(trimmed, '\n'); idx >= 0 {
		lastLine = trimmed[idx+1:]
	}
	lead := strings.TrimLeft(lastLine, " \t")
	return strings.HasPrefix(lead, ideFooterTag)
}

func patchShellDesktopContent(content string) string {
	out := reDesktopIneligibleCase.ReplaceAllString(content, `?.failureDetails?.case==="NEVER_MATCH"?this._authActor.send({type:"SET_INELIGIBLE"`)
	out = strings.ReplaceAll(out, "ineligible", FieldToken)

	out = replaceBySubmatch(reDesktopUserStatusCheck, out, func(src string, m []int) (string, bool) {
		var1 := src[m[2]:m[3]]
		var2 := src[m[4]:m[5]]
		if var1 != var2 {
			return "", false
		}
		matched := src[m[0]:m[1]]
		inject := fmt.Sprintf(`%s={"planStatus":{"planInfo":{"planName":"pro"}},"disableTelemetry":false,"userDataCollectionForceDisabled":false};if(%s){`, var1, var1)
		return strings.Replace(matched, "if("+var1+"){", inject, 1), true
	})

	out = replaceBySubmatch(reDesktopErrorHandler, out, func(src string, m []int) (string, bool) {
		param1 := src[m[2]:m[3]]
		localVar := src[m[4]:m[5]]
		param2 := src[m[6]:m[7]]
		if param1 != param2 {
			return "", false
		}
		return fmt.Sprintf(`_handleAuthErrorResponse(%s){var %s=%s?.failureDetails;if(%s?.case==="%s"){this._authActor.send({type:"AUTH_SUCCESS",tokenInfo:{accessToken:""},scopes:[],isGcpTos:false});return;}`,
			param1, localVar, param1, localVar, FieldToken), true
	})

	return reDesktopEligibleFlag.ReplaceAllString(out, `${1}true`)
}

func replaceBySubmatch(re *regexp.Regexp, src string, fn func(s string, loc []int) (string, bool)) string {
	locs := re.FindAllStringSubmatchIndex(src, -1)
	if len(locs) == 0 {
		return src
	}
	var b strings.Builder
	b.Grow(len(src) + len(locs)*64)
	last := 0
	for _, m := range locs {
		repl, ok := fn(src, m)
		if !ok {
			continue
		}
		b.WriteString(src[last:m[0]])
		b.WriteString(repl)
		last = m[1]
	}
	b.WriteString(src[last:])
	return b.String()
}
