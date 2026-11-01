//go:build darwin

package daemon

import (
	"crypto/ed25519"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"
)

const (
	keychainService       = "ai.true-line.agent-bot-machine"
	keychainAccount       = "daemon.key"
	errSecItemNotFound    = -25300
	errSecDuplicateItem   = -25299
	kCFStringEncodingUTF8 = 0x08000100
	rtldDefault           = ^uintptr(1) // RTLD_DEFAULT (-2) on Darwin
)

// Implemented in the runtime package (runtime/sys_darwin.go).
func syscall_syscall(fn, a1, a2, a3 uintptr) (r1, r2 uintptr, err syscall.Errno)
func syscall_syscall6(fn, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err syscall.Errno)

//go:linkname syscall_syscall syscall.syscall
//go:linkname syscall_syscall6 syscall.syscall6

//go:cgo_import_dynamic kc_dlsym dlsym "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic kc_CFDataCreate CFDataCreate "/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation"
//go:cgo_import_dynamic kc_CFDataGetLength CFDataGetLength "/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation"
//go:cgo_import_dynamic kc_CFDataGetBytes CFDataGetBytes "/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation"
//go:cgo_import_dynamic kc_CFNumberCreate CFNumberCreate "/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation"
//go:cgo_import_dynamic kc_CFStringCreateWithBytes CFStringCreateWithBytes "/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation"
//go:cgo_import_dynamic kc_CFDictionaryCreateMutable CFDictionaryCreateMutable "/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation"
//go:cgo_import_dynamic kc_CFDictionarySetValue CFDictionarySetValue "/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation"
//go:cgo_import_dynamic kc_CFArrayCreate CFArrayCreate "/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation"
//go:cgo_import_dynamic kc_CFRelease CFRelease "/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation"
//go:cgo_import_dynamic kc_SecItemCopyMatching SecItemCopyMatching "/System/Library/Frameworks/Security.framework/Versions/A/Security"
//go:cgo_import_dynamic kc_SecItemAdd SecItemAdd "/System/Library/Frameworks/Security.framework/Versions/A/Security"
//go:cgo_import_dynamic kc_SecItemDelete SecItemDelete "/System/Library/Frameworks/Security.framework/Versions/A/Security"
//go:cgo_import_dynamic kc_SecTrustedApplicationCreateFromPath SecTrustedApplicationCreateFromPath "/System/Library/Frameworks/Security.framework/Versions/A/Security"
//go:cgo_import_dynamic kc_SecAccessCreate SecAccessCreate "/System/Library/Frameworks/Security.framework/Versions/A/Security"

var (
	kc_dlsym_trampoline_addr                               uintptr
	kc_CFDataCreate_trampoline_addr                        uintptr
	kc_CFDataGetLength_trampoline_addr                     uintptr
	kc_CFDataGetBytes_trampoline_addr                      uintptr
	kc_CFNumberCreate_trampoline_addr                      uintptr
	kc_CFStringCreateWithBytes_trampoline_addr             uintptr
	kc_CFDictionaryCreateMutable_trampoline_addr           uintptr
	kc_CFDictionarySetValue_trampoline_addr                uintptr
	kc_CFArrayCreate_trampoline_addr                       uintptr
	kc_CFRelease_trampoline_addr                           uintptr
	kc_SecItemCopyMatching_trampoline_addr                 uintptr
	kc_SecItemAdd_trampoline_addr                          uintptr
	kc_SecItemDelete_trampoline_addr                       uintptr
	kc_SecTrustedApplicationCreateFromPath_trampoline_addr uintptr
	kc_SecAccessCreate_trampoline_addr                     uintptr
)

func cfSymAddr(name string) uintptr {
	b := append([]byte(name), 0)
	p := unsafe.Pointer(unsafe.SliceData(b))
	r1, _, _ := syscall_syscall(kc_dlsym_trampoline_addr, rtldDefault, uintptr(p), 0)
	runtime.KeepAlive(b)
	return r1
}

func cfTrue() uintptr {
	one := int32(1)
	const kCFNumberSInt32Type = 3
	r1, _, _ := syscall_syscall(kc_CFNumberCreate_trampoline_addr, 0, kCFNumberSInt32Type, uintptr(unsafe.Pointer(&one)))
	runtime.KeepAlive(&one)
	return r1
}

func cfString(s string) uintptr {
	p := unsafe.Pointer(unsafe.StringData(s))
	r1, _, _ := syscall_syscall6(kc_CFStringCreateWithBytes_trampoline_addr, 0, uintptr(p), uintptr(len(s)), kCFStringEncodingUTF8, 0, 0)
	runtime.KeepAlive(s)
	return r1
}

func cfData(b []byte) uintptr {
	var p unsafe.Pointer
	if len(b) > 0 {
		p = unsafe.Pointer(unsafe.SliceData(b))
	}
	r1, _, _ := syscall_syscall(kc_CFDataCreate_trampoline_addr, 0, uintptr(p), uintptr(len(b)))
	runtime.KeepAlive(b)
	return r1
}

func cfDataBytes(ref uintptr) []byte {
	n, _, _ := syscall_syscall(kc_CFDataGetLength_trampoline_addr, ref, 0, 0)
	if int(n) <= 0 {
		return nil
	}
	out := make([]byte, int(n))
	p := unsafe.Pointer(unsafe.SliceData(out))
	syscall_syscall6(kc_CFDataGetBytes_trampoline_addr, ref, 0, n, uintptr(p), 0, 0)
	runtime.KeepAlive(out)
	return out
}

func cfRelease(ref uintptr) {
	if ref != 0 {
		syscall_syscall(kc_CFRelease_trampoline_addr, ref, 0, 0)
	}
}

func cfNewDict() uintptr {
	keyCB := cfSymAddr("kCFTypeDictionaryKeyCallBacks")
	valCB := cfSymAddr("kCFTypeDictionaryValueCallBacks")
	r1, _, _ := syscall_syscall6(kc_CFDictionaryCreateMutable_trampoline_addr, 0, 0, keyCB, valCB, 0, 0)
	return r1
}

func cfDictSet(dict, key, val uintptr) {
	if dict != 0 && key != 0 && val != 0 {
		syscall_syscall(kc_CFDictionarySetValue_trampoline_addr, dict, key, val)
	}
}

func createSelfOnlyAccess(label string) (uintptr, error) {
	var selfApp uintptr
	r1, _, _ := syscall_syscall(kc_SecTrustedApplicationCreateFromPath_trampoline_addr, 0, uintptr(unsafe.Pointer(&selfApp)), 0)
	if status := int32(r1); status != 0 {
		return 0, fmt.Errorf("SecTrustedApplicationCreateFromPath: OSStatus %d", status)
	}
	defer cfRelease(selfApp)
	items := [1]uintptr{selfApp}
	arrCB := cfSymAddr("kCFTypeArrayCallBacks")
	trustedList, _, _ := syscall_syscall6(kc_CFArrayCreate_trampoline_addr, 0, uintptr(unsafe.Pointer(&items[0])), 1, arrCB, 0, 0)
	runtime.KeepAlive(items)
	if trustedList == 0 {
		return 0, fmt.Errorf("CFArrayCreate failed")
	}
	defer cfRelease(trustedList)
	desc := cfString(label)
	defer cfRelease(desc)
	var accessRef uintptr
	r2, _, _ := syscall_syscall(kc_SecAccessCreate_trampoline_addr, desc, trustedList, uintptr(unsafe.Pointer(&accessRef)))
	if status := int32(r2); status != 0 {
		return 0, fmt.Errorf("SecAccessCreate: OSStatus %d", status)
	}
	return accessRef, nil
}

type darwinKeychainStore struct {
	service string
	account string
}

func defaultKeyStore(path string) keyStore {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	want := filepath.Join(home, ".config", "agent-machine", "daemon.key")
	if filepath.Clean(path) != filepath.Clean(want) {
		return nil
	}
	return darwinKeychainStore{service: keychainService, account: keychainAccount}
}

func (s darwinKeychainStore) baseQuery() (uintptr, []uintptr) {
	dict := cfNewDict()
	kClass := cfString("class")
	vClass := cfString("genp")
	kSvc := cfString("svce")
	vSvc := cfString(s.service)
	kAcct := cfString("acct")
	vAcct := cfString(s.account)
	cfDictSet(dict, kClass, vClass)
	cfDictSet(dict, kSvc, vSvc)
	cfDictSet(dict, kAcct, vAcct)
	return dict, []uintptr{dict, kClass, vClass, kSvc, vSvc, kAcct, vAcct}
}

func releaseAll(refs []uintptr) {
	for _, r := range refs {
		cfRelease(r)
	}
}

// Load reads the 32-byte Ed25519 seed from the macOS Keychain in-process via
// SecItemCopyMatching. Only errSecItemNotFound (-25300) returns (nil, false, nil).
// Any other status (such as a locked keychain or denied access) fails closed
// with an error so the caller never rotates the key.
func (s darwinKeychainStore) Load() ([]byte, bool, error) {
	query, refs := s.baseQuery()
	kRet := cfString("r_Data")
	vRet := cfTrue()
	kLim := cfString("m_Limit")
	vLim := cfString("m_LimitOne")
	refs = append(refs, kRet, vRet, kLim, vLim)
	defer releaseAll(refs)
	cfDictSet(query, kRet, vRet)
	cfDictSet(query, kLim, vLim)
	var result uintptr
	r1, _, _ := syscall_syscall(kc_SecItemCopyMatching_trampoline_addr, query, uintptr(unsafe.Pointer(&result)), 0)
	status := int32(r1)
	if status == errSecItemNotFound {
		return nil, false, nil
	}
	if status != 0 {
		return nil, false, fmt.Errorf("keychain SecItemCopyMatching failed: OSStatus %d", status)
	}
	defer cfRelease(result)
	seed := cfDataBytes(result)
	if len(seed) != ed25519.SeedSize {
		return nil, false, fmt.Errorf("keychain seed has invalid length %d", len(seed))
	}
	return seed, true, nil
}

// Save stores the 32-byte Ed25519 seed in-process via SecItemAdd with
// kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly and a SecAccessRef that
// trusts only this binary.
func (s darwinKeychainStore) Save(seed []byte) error {
	if len(seed) != ed25519.SeedSize {
		return os.ErrInvalid
	}
	attrs, refs := s.baseQuery()
	kPdmn := cfString("pdmn")
	vPdmn := cfString("cku") // kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly
	kData := cfString("v_Data")
	vData := cfData(seed)
	refs = append(refs, kPdmn, vPdmn, kData, vData)
	cfDictSet(attrs, kPdmn, vPdmn)
	cfDictSet(attrs, kData, vData)
	access, err := createSelfOnlyAccess(s.service)
	if err != nil {
		releaseAll(refs)
		return err
	}
	kAcc := cfString("acls")
	refs = append(refs, kAcc, access)
	cfDictSet(attrs, kAcc, access)
	defer releaseAll(refs)
	r1, _, _ := syscall_syscall(kc_SecItemAdd_trampoline_addr, attrs, 0, 0)
	status := int32(r1)
	if status == errSecDuplicateItem {
		if err := s.Delete(); err != nil {
			return err
		}
		r2, _, _ := syscall_syscall(kc_SecItemAdd_trampoline_addr, attrs, 0, 0)
		status = int32(r2)
	}
	if status != 0 {
		return fmt.Errorf("keychain SecItemAdd failed: OSStatus %d", status)
	}
	return nil
}

func (s darwinKeychainStore) Delete() error {
	query, refs := s.baseQuery()
	defer releaseAll(refs)
	r1, _, _ := syscall_syscall(kc_SecItemDelete_trampoline_addr, query, 0, 0)
	status := int32(r1)
	if status != 0 && status != errSecItemNotFound {
		return fmt.Errorf("keychain SecItemDelete failed: OSStatus %d", status)
	}
	return nil
}
