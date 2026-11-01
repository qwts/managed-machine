//go:build darwin

package daemon

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"
)

const (
	keychainService          = "ai.true-line.agent-bot-machine"
	keychainAccount          = "daemon.key"
	keychainAccessGroup      = "Z5DM34QS5U.ai.true-line.agent-bot-machine"
	enclaveWrapTag           = "ai.true-line.agent-bot-machine.wrap"
	eciesAlgorithm           = "algid:encrypt:ECIES:ECDH:KDFX963:SHA256:AESGCM"
	errSecItemNotFound       = -25300
	errSecDuplicateItem      = -25299
	errSecMissingEntitlement = -34018
	errSecNotAvailable       = -25291
	errSecUnimplemented      = -4
	kCFStringEncodingUTF8    = 0x08000100
	rtldDefault              = ^uintptr(1) // RTLD_DEFAULT (-2) on Darwin
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
//go:cgo_import_dynamic kc_CFPropertyListCreateWithData CFPropertyListCreateWithData "/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation"
//go:cgo_import_dynamic kc_CFStringCreateWithBytes CFStringCreateWithBytes "/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation"
//go:cgo_import_dynamic kc_CFDictionaryCreateMutable CFDictionaryCreateMutable "/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation"
//go:cgo_import_dynamic kc_CFDictionarySetValue CFDictionarySetValue "/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation"
//go:cgo_import_dynamic kc_CFArrayCreate CFArrayCreate "/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation"
//go:cgo_import_dynamic kc_CFRelease CFRelease "/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation"
//go:cgo_import_dynamic kc_SecItemCopyMatching SecItemCopyMatching "/System/Library/Frameworks/Security.framework/Versions/A/Security"
//go:cgo_import_dynamic kc_SecItemAdd SecItemAdd "/System/Library/Frameworks/Security.framework/Versions/A/Security"
//go:cgo_import_dynamic kc_SecItemUpdate SecItemUpdate "/System/Library/Frameworks/Security.framework/Versions/A/Security"
//go:cgo_import_dynamic kc_SecItemDelete SecItemDelete "/System/Library/Frameworks/Security.framework/Versions/A/Security"
//go:cgo_import_dynamic kc_SecKeyCreateRandomKey SecKeyCreateRandomKey "/System/Library/Frameworks/Security.framework/Versions/A/Security"
//go:cgo_import_dynamic kc_SecKeyCopyPublicKey SecKeyCopyPublicKey "/System/Library/Frameworks/Security.framework/Versions/A/Security"
//go:cgo_import_dynamic kc_SecKeyCreateEncryptedData SecKeyCreateEncryptedData "/System/Library/Frameworks/Security.framework/Versions/A/Security"
//go:cgo_import_dynamic kc_SecKeyCreateDecryptedData SecKeyCreateDecryptedData "/System/Library/Frameworks/Security.framework/Versions/A/Security"
//go:cgo_import_dynamic kc_SecKeychainCreate SecKeychainCreate "/System/Library/Frameworks/Security.framework/Versions/A/Security"
//go:cgo_import_dynamic kc_SecKeychainOpen SecKeychainOpen "/System/Library/Frameworks/Security.framework/Versions/A/Security"
//go:cgo_import_dynamic kc_SecKeychainUnlock SecKeychainUnlock "/System/Library/Frameworks/Security.framework/Versions/A/Security"
//go:cgo_import_dynamic kc_SecKeychainDelete SecKeychainDelete "/System/Library/Frameworks/Security.framework/Versions/A/Security"

var (
	kc_dlsym_trampoline_addr                        uintptr
	kc_CFDataCreate_trampoline_addr                 uintptr
	kc_CFDataGetLength_trampoline_addr              uintptr
	kc_CFDataGetBytes_trampoline_addr               uintptr
	kc_CFNumberCreate_trampoline_addr               uintptr
	kc_CFPropertyListCreateWithData_trampoline_addr uintptr
	kc_CFStringCreateWithBytes_trampoline_addr      uintptr
	kc_CFDictionaryCreateMutable_trampoline_addr    uintptr
	kc_CFDictionarySetValue_trampoline_addr         uintptr
	kc_CFArrayCreate_trampoline_addr                uintptr
	kc_CFRelease_trampoline_addr                    uintptr
	kc_SecItemCopyMatching_trampoline_addr          uintptr
	kc_SecItemAdd_trampoline_addr                   uintptr
	kc_SecItemUpdate_trampoline_addr                uintptr
	kc_SecItemDelete_trampoline_addr                uintptr
	kc_SecKeyCreateRandomKey_trampoline_addr        uintptr
	kc_SecKeyCopyPublicKey_trampoline_addr          uintptr
	kc_SecKeyCreateEncryptedData_trampoline_addr    uintptr
	kc_SecKeyCreateDecryptedData_trampoline_addr    uintptr
	kc_SecKeychainCreate_trampoline_addr            uintptr
	kc_SecKeychainOpen_trampoline_addr              uintptr
	kc_SecKeychainUnlock_trampoline_addr            uintptr
	kc_SecKeychainDelete_trampoline_addr            uintptr
)

func cfSymAddr(name string) uintptr {
	b := append([]byte(name), 0)
	p := unsafe.Pointer(unsafe.SliceData(b))
	r1, _, _ := syscall_syscall(kc_dlsym_trampoline_addr, rtldDefault, uintptr(p), 0)
	runtime.KeepAlive(b)
	return r1
}

func cfInt32(v int32) uintptr {
	val := v
	const kCFNumberSInt32Type = 3
	r1, _, _ := syscall_syscall(kc_CFNumberCreate_trampoline_addr, 0, kCFNumberSInt32Type, uintptr(unsafe.Pointer(&val)))
	runtime.KeepAlive(&val)
	return r1
}

func cfTrue() uintptr {
	d := cfData([]byte(`<plist version="1.0"><true/></plist>`))
	defer cfRelease(d)
	r1, _, _ := syscall_syscall6(kc_CFPropertyListCreateWithData_trampoline_addr, 0, d, 0, 0, 0, 0)
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

func releaseAll(refs []uintptr) {
	for _, r := range refs {
		cfRelease(r)
	}
}

type darwinKeychainStore struct {
	service      string
	account      string
	accessGroup  string
	keychainPath string
}

// defaultKeyStore always returns a darwinKeychainStore on macOS so no path on
// Darwin ever falls back to a plaintext daemon.key file on disk.
func defaultKeyStore(path string) keyStore {
	dir := filepath.Dir(filepath.Clean(path))
	return darwinKeychainStore{
		service:      keychainService,
		account:      keychainAccount,
		accessGroup:  keychainAccessGroup,
		keychainPath: filepath.Join(dir, "daemon.keychain-db"),
	}
}

// lookupOrCreateEnclaveKey looks up or generates a P-256 key inside the
// Apple Secure Enclave (kSecAttrTokenIDSecureEnclave) under the Data
// Protection Keychain access group to wrap/unwrap the Ed25519 seed.
func (s darwinKeychainStore) lookupOrCreateEnclaveKey(create bool) (uintptr, error) {
	query := cfNewDict()
	kClass := cfString("class")
	vClass := cfString("keys")
	kType := cfString("type")
	vType := cfString("73") // kSecAttrKeyTypeECSECPrimeRandom
	kKcls := cfString("kcls")
	vKcls := cfString("1") // kSecAttrKeyClassPrivate
	kTag := cfString("atag")
	vTag := cfData([]byte(enclaveWrapTag))
	kNleg := cfString("nleg")
	vTrue := cfTrue()
	kAgrp := cfString("agrp")
	vAgrp := cfString(s.accessGroup)
	kRetRef := cfString("r_Ref")
	refs := []uintptr{query, kClass, vClass, kType, vType, kKcls, vKcls, kTag, vTag, kNleg, vTrue, kAgrp, vAgrp, kRetRef}
	defer releaseAll(refs)

	cfDictSet(query, kClass, vClass)
	cfDictSet(query, kType, vType)
	cfDictSet(query, kKcls, vKcls)
	cfDictSet(query, kTag, vTag)
	cfDictSet(query, kNleg, vTrue)
	cfDictSet(query, kAgrp, vAgrp)
	cfDictSet(query, kRetRef, vTrue)

	var keyRef uintptr
	r1, _, _ := syscall_syscall(kc_SecItemCopyMatching_trampoline_addr, query, uintptr(unsafe.Pointer(&keyRef)), 0)
	status := int32(r1)
	if status == 0 && keyRef != 0 {
		return keyRef, nil
	}
	if status != errSecItemNotFound || !create {
		return 0, fmt.Errorf("Secure Enclave key lookup failed: OSStatus %d", status)
	}

	privAttrs := cfNewDict()
	kPerm := cfString("perm")
	kPdmn := cfString("pdmn")
	vPdmn := cfString("cku") // kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly
	cfDictSet(privAttrs, kPerm, vTrue)
	cfDictSet(privAttrs, kTag, vTag)
	cfDictSet(privAttrs, kPdmn, vPdmn)

	attrs := cfNewDict()
	kBsiz := cfString("bsiz")
	vBsiz := cfInt32(256)
	kTkid := cfString("tkid")
	vTkid := cfString("com.apple.setoken")
	kPriv := cfString("private")
	refs = append(refs, privAttrs, kPerm, kPdmn, vPdmn, attrs, kBsiz, vBsiz, kTkid, vTkid, kPriv)

	cfDictSet(attrs, kType, vType)
	cfDictSet(attrs, kBsiz, vBsiz)
	cfDictSet(attrs, kTkid, vTkid)
	cfDictSet(attrs, kNleg, vTrue)
	cfDictSet(attrs, kAgrp, vAgrp)
	cfDictSet(attrs, kPriv, privAttrs)

	var errRef uintptr
	newKey, _, _ := syscall_syscall(kc_SecKeyCreateRandomKey_trampoline_addr, attrs, uintptr(unsafe.Pointer(&errRef)), 0)
	if errRef != 0 {
		cfRelease(errRef)
	}
	if newKey == 0 {
		return 0, fmt.Errorf("SecKeyCreateRandomKey in Secure Enclave failed")
	}
	return newKey, nil
}

func (s darwinKeychainStore) wrapWithEnclave(seed []byte) ([]byte, bool) {
	privKey, err := s.lookupOrCreateEnclaveKey(true)
	if err != nil || privKey == 0 {
		return nil, false
	}
	defer cfRelease(privKey)
	pubKey, _, _ := syscall_syscall(kc_SecKeyCopyPublicKey_trampoline_addr, privKey, 0, 0)
	if pubKey == 0 {
		return nil, false
	}
	defer cfRelease(pubKey)
	alg := cfString(eciesAlgorithm)
	defer cfRelease(alg)
	plainRef := cfData(seed)
	defer cfRelease(plainRef)
	var errRef uintptr
	cipherRef, _, _ := syscall_syscall6(kc_SecKeyCreateEncryptedData_trampoline_addr, pubKey, alg, plainRef, uintptr(unsafe.Pointer(&errRef)), 0, 0)
	if errRef != 0 {
		cfRelease(errRef)
	}
	if cipherRef == 0 {
		return nil, false
	}
	defer cfRelease(cipherRef)
	return cfDataBytes(cipherRef), true
}

func (s darwinKeychainStore) unwrapWithEnclave(ciphertext []byte) ([]byte, error) {
	privKey, err := s.lookupOrCreateEnclaveKey(false)
	if err != nil || privKey == 0 {
		return nil, fmt.Errorf("Secure Enclave wrapping key unavailable: %w", err)
	}
	defer cfRelease(privKey)
	alg := cfString(eciesAlgorithm)
	defer cfRelease(alg)
	cipherRef := cfData(ciphertext)
	defer cfRelease(cipherRef)
	var errRef uintptr
	plainRef, _, _ := syscall_syscall6(kc_SecKeyCreateDecryptedData_trampoline_addr, privKey, alg, cipherRef, uintptr(unsafe.Pointer(&errRef)), 0, 0)
	if errRef != 0 {
		cfRelease(errRef)
	}
	if plainRef == 0 {
		return nil, fmt.Errorf("SecKeyCreateDecryptedData failed")
	}
	defer cfRelease(plainRef)
	return cfDataBytes(plainRef), nil
}

func (s darwinKeychainStore) openDedicatedKeychain(create bool) (uintptr, uintptr, error) {
	if s.keychainPath == "" {
		return 0, 0, errors.New("dedicated keychain path is required")
	}
	dir := filepath.Dir(s.keychainPath)
	if create {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return 0, 0, err
		}
		_ = stripDarwinACL(dir)
		_ = os.Chmod(dir, 0o700)
	}
	cpath := append([]byte(s.keychainPath), 0)
	p := unsafe.Pointer(unsafe.SliceData(cpath))
	empty := [1]byte{0}
	ep := unsafe.Pointer(&empty[0])
	var kc uintptr
	if _, err := os.Lstat(s.keychainPath); errors.Is(err, os.ErrNotExist) {
		if !create {
			return 0, 0, nil
		}
		r1, _, _ := syscall_syscall6(kc_SecKeychainCreate_trampoline_addr, uintptr(p), 0, uintptr(ep), 0, 0, uintptr(unsafe.Pointer(&kc)))
		runtime.KeepAlive(cpath)
		runtime.KeepAlive(empty)
		if status := int32(r1); status != 0 {
			return 0, 0, fmt.Errorf("SecKeychainCreate failed: OSStatus %d", status)
		}
		_ = stripDarwinACL(s.keychainPath)
		_ = os.Chmod(s.keychainPath, 0o600)
	} else if err != nil {
		return 0, 0, err
	} else {
		r1, _, _ := syscall_syscall(kc_SecKeychainOpen_trampoline_addr, uintptr(p), uintptr(unsafe.Pointer(&kc)), 0)
		runtime.KeepAlive(cpath)
		if status := int32(r1); status != 0 {
			return 0, 0, fmt.Errorf("SecKeychainOpen failed: OSStatus %d", status)
		}
		r2, _, _ := syscall_syscall6(kc_SecKeychainUnlock_trampoline_addr, kc, 0, uintptr(ep), 1, 0, 0)
		runtime.KeepAlive(empty)
		if status := int32(r2); status != 0 {
			cfRelease(kc)
			return 0, 0, fmt.Errorf("SecKeychainUnlock failed: OSStatus %d", status)
		}
	}
	items := [1]uintptr{kc}
	arrCB := cfSymAddr("kCFTypeArrayCallBacks")
	searchList, _, _ := syscall_syscall6(kc_CFArrayCreate_trampoline_addr, 0, uintptr(unsafe.Pointer(&items[0])), 1, arrCB, 0, 0)
	runtime.KeepAlive(items)
	return kc, searchList, nil
}

func (s darwinKeychainStore) dataProtectionQuery() (uintptr, []uintptr) {
	dict := cfNewDict()
	kClass := cfString("class")
	vClass := cfString("genp")
	kSvc := cfString("svce")
	vSvc := cfString(s.service)
	kAcct := cfString("acct")
	vAcct := cfString(s.account)
	kNleg := cfString("nleg")
	vTrue := cfTrue()
	kAgrp := cfString("agrp")
	vAgrp := cfString(s.accessGroup)
	cfDictSet(dict, kClass, vClass)
	cfDictSet(dict, kSvc, vSvc)
	cfDictSet(dict, kAcct, vAcct)
	cfDictSet(dict, kNleg, vTrue)
	cfDictSet(dict, kAgrp, vAgrp)
	return dict, []uintptr{dict, kClass, vClass, kSvc, vSvc, kAcct, vAcct, kNleg, vTrue, kAgrp, vAgrp}
}

func (s darwinKeychainStore) dedicatedQuery(kc, searchList uintptr, includeUseKeychain bool) (uintptr, []uintptr) {
	dict := cfNewDict()
	kClass := cfString("class")
	vClass := cfString("genp")
	kSvc := cfString("svce")
	vSvc := cfString(s.service)
	kAcct := cfString("acct")
	vAcct := cfString(s.account)
	kSearch := cfString("m_SearchList")
	refs := []uintptr{dict, kClass, vClass, kSvc, vSvc, kAcct, vAcct, kSearch}
	cfDictSet(dict, kClass, vClass)
	cfDictSet(dict, kSvc, vSvc)
	cfDictSet(dict, kAcct, vAcct)
	cfDictSet(dict, kSearch, searchList)
	if includeUseKeychain && kc != 0 {
		kUse := cfString("u_Keychain")
		refs = append(refs, kUse)
		cfDictSet(dict, kUse, kc)
	}
	return dict, refs
}

// Load reads the Ed25519 seed in-process via SecItemCopyMatching. It tries the
// Data Protection Keychain (and Secure Enclave unwrap when wrapped) first, and
// uses the dedicated admin keychain only when Data Protection Keychain returns
// errSecMissingEntitlement (-34018) on an un-provisioned build. Only
// errSecItemNotFound (-25300) returns (nil, false, nil); any other error fails
// closed without rotating the key.
func (s darwinKeychainStore) Load() ([]byte, bool, error) {
	dpQuery, dpRefs := s.dataProtectionQuery()
	kRet := cfString("r_Data")
	vTrue := cfTrue()
	kLim := cfString("m_Limit")
	vLim := cfString("m_LimitOne")
	dpRefs = append(dpRefs, kRet, vTrue, kLim, vLim)
	cfDictSet(dpQuery, kRet, vTrue)
	cfDictSet(dpQuery, kLim, vLim)
	var result uintptr
	r1, _, _ := syscall_syscall(kc_SecItemCopyMatching_trampoline_addr, dpQuery, uintptr(unsafe.Pointer(&result)), 0)
	releaseAll(dpRefs)
	status := int32(r1)
	if status == 0 {
		defer cfRelease(result)
		raw := cfDataBytes(result)
		return s.decodePayload(raw)
	}
	if status != errSecItemNotFound && status != errSecMissingEntitlement {
		return nil, false, fmt.Errorf("keychain SecItemCopyMatching failed: OSStatus %d", status)
	}
	kc, searchList, err := s.openDedicatedKeychain(false)
	if err != nil {
		return nil, false, err
	}
	if kc == 0 {
		return nil, false, nil
	}
	defer cfRelease(kc)
	defer cfRelease(searchList)
	q, refs := s.dedicatedQuery(kc, searchList, false)
	kRet2 := cfString("r_Data")
	vTrue2 := cfTrue()
	kLim2 := cfString("m_Limit")
	vLim2 := cfString("m_LimitOne")
	refs = append(refs, kRet2, vTrue2, kLim2, vLim2)
	defer releaseAll(refs)
	cfDictSet(q, kRet2, vTrue2)
	cfDictSet(q, kLim2, vLim2)
	var res2 uintptr
	r2, _, _ := syscall_syscall(kc_SecItemCopyMatching_trampoline_addr, q, uintptr(unsafe.Pointer(&res2)), 0)
	st2 := int32(r2)
	if st2 == errSecItemNotFound {
		return nil, false, nil
	}
	if st2 != 0 {
		return nil, false, fmt.Errorf("dedicated keychain SecItemCopyMatching failed: OSStatus %d", st2)
	}
	defer cfRelease(res2)
	raw := cfDataBytes(res2)
	return s.decodePayload(raw)
}

func (s darwinKeychainStore) decodePayload(raw []byte) ([]byte, bool, error) {
	if len(raw) == ed25519.SeedSize {
		return raw, true, nil
	}
	if len(raw) > 4 && string(raw[:4]) == "SEW1" {
		defer zeroBytes(raw)
		seed, err := s.unwrapWithEnclave(raw[4:])
		if err != nil {
			return nil, false, err
		}
		if len(seed) != ed25519.SeedSize {
			zeroBytes(seed)
			return nil, false, fmt.Errorf("unwrapped seed has invalid length %d", len(seed))
		}
		return seed, true, nil
	}
	zeroBytes(raw)
	return nil, false, fmt.Errorf("keychain item has invalid length %d", len(raw))
}

// Save stores the 32-byte Ed25519 seed in-process, wrapping it with a Secure
// Enclave P-256 key when available, setting
// kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly, and updating in place with
// SecItemUpdate when an item already exists so a failed write never loses the
// previous key.
func (s darwinKeychainStore) Save(seed []byte) error {
	if len(seed) != ed25519.SeedSize {
		return os.ErrInvalid
	}
	payload := seed
	if wrapped, ok := s.wrapWithEnclave(seed); ok {
		payload = append([]byte("SEW1"), wrapped...)
		defer zeroBytes(payload)
	}
	dpAttrs, dpRefs := s.dataProtectionQuery()
	kPdmn := cfString("pdmn")
	vPdmn := cfString("cku") // kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly
	kData := cfString("v_Data")
	vData := cfData(payload)
	dpRefs = append(dpRefs, kPdmn, vPdmn, kData, vData)
	cfDictSet(dpAttrs, kPdmn, vPdmn)
	cfDictSet(dpAttrs, kData, vData)
	r1, _, _ := syscall_syscall(kc_SecItemAdd_trampoline_addr, dpAttrs, 0, 0)
	releaseAll(dpRefs)
	status := int32(r1)
	if status == 0 {
		return nil
	}
	if status == errSecDuplicateItem {
		matchQ, mRefs := s.dataProtectionQuery()
		upd := cfNewDict()
		ukData := cfString("v_Data")
		uvData := cfData(payload)
		mRefs = append(mRefs, upd, ukData, uvData)
		cfDictSet(upd, ukData, uvData)
		ru, _, _ := syscall_syscall(kc_SecItemUpdate_trampoline_addr, matchQ, upd, 0)
		releaseAll(mRefs)
		if ust := int32(ru); ust != 0 {
			return fmt.Errorf("keychain SecItemUpdate failed: OSStatus %d", ust)
		}
		return nil
	}
	if status != errSecMissingEntitlement {
		return fmt.Errorf("keychain SecItemAdd failed: OSStatus %d", status)
	}

	kc, searchList, err := s.openDedicatedKeychain(true)
	if err != nil {
		return err
	}
	defer cfRelease(kc)
	defer cfRelease(searchList)
	q, refs := s.dedicatedQuery(kc, searchList, true)
	kPdmn2 := cfString("pdmn")
	vPdmn2 := cfString("cku")
	kData2 := cfString("v_Data")
	vData2 := cfData(payload)
	refs = append(refs, kPdmn2, vPdmn2, kData2, vData2)
	cfDictSet(q, kPdmn2, vPdmn2)
	cfDictSet(q, kData2, vData2)
	r2, _, _ := syscall_syscall(kc_SecItemAdd_trampoline_addr, q, 0, 0)
	releaseAll(refs)
	st2 := int32(r2)
	if st2 == 0 {
		return nil
	}
	if st2 == errSecDuplicateItem {
		matchQ, mRefs := s.dedicatedQuery(kc, searchList, false)
		upd := cfNewDict()
		ukData := cfString("v_Data")
		uvData := cfData(payload)
		mRefs = append(mRefs, upd, ukData, uvData)
		cfDictSet(upd, ukData, uvData)
		ru, _, _ := syscall_syscall(kc_SecItemUpdate_trampoline_addr, matchQ, upd, 0)
		releaseAll(mRefs)
		if ust := int32(ru); ust != 0 {
			return fmt.Errorf("dedicated keychain SecItemUpdate failed: OSStatus %d", ust)
		}
		return nil
	}
	return fmt.Errorf("dedicated keychain SecItemAdd failed: OSStatus %d", st2)
}

func (s darwinKeychainStore) Delete() error {
	dpQuery, dpRefs := s.dataProtectionQuery()
	r1, _, _ := syscall_syscall(kc_SecItemDelete_trampoline_addr, dpQuery, 0, 0)
	releaseAll(dpRefs)
	status := int32(r1)
	if status != 0 && status != errSecItemNotFound && status != errSecMissingEntitlement {
		return fmt.Errorf("keychain SecItemDelete failed: OSStatus %d", status)
	}
	kc, searchList, err := s.openDedicatedKeychain(false)
	if err != nil || kc == 0 {
		return err
	}
	defer cfRelease(kc)
	defer cfRelease(searchList)
	q, refs := s.dedicatedQuery(kc, searchList, false)
	r2, _, _ := syscall_syscall(kc_SecItemDelete_trampoline_addr, q, 0, 0)
	releaseAll(refs)
	st2 := int32(r2)
	if st2 != 0 && st2 != errSecItemNotFound {
		return fmt.Errorf("dedicated keychain SecItemDelete failed: OSStatus %d", st2)
	}
	return nil
}
