//go:build darwin

#include "textflag.h"

TEXT kc_dlsym_trampoline<>(SB),NOSPLIT,$0-0
	JMP	kc_dlsym(SB)
GLOBL	·kc_dlsym_trampoline_addr(SB), RODATA, $8
DATA	·kc_dlsym_trampoline_addr(SB)/8, $kc_dlsym_trampoline<>(SB)

TEXT kc_CFDataCreate_trampoline<>(SB),NOSPLIT,$0-0
	JMP	kc_CFDataCreate(SB)
GLOBL	·kc_CFDataCreate_trampoline_addr(SB), RODATA, $8
DATA	·kc_CFDataCreate_trampoline_addr(SB)/8, $kc_CFDataCreate_trampoline<>(SB)

TEXT kc_CFDataGetLength_trampoline<>(SB),NOSPLIT,$0-0
	JMP	kc_CFDataGetLength(SB)
GLOBL	·kc_CFDataGetLength_trampoline_addr(SB), RODATA, $8
DATA	·kc_CFDataGetLength_trampoline_addr(SB)/8, $kc_CFDataGetLength_trampoline<>(SB)

TEXT kc_CFDataGetBytes_trampoline<>(SB),NOSPLIT,$0-0
	JMP	kc_CFDataGetBytes(SB)
GLOBL	·kc_CFDataGetBytes_trampoline_addr(SB), RODATA, $8
DATA	·kc_CFDataGetBytes_trampoline_addr(SB)/8, $kc_CFDataGetBytes_trampoline<>(SB)

TEXT kc_CFNumberCreate_trampoline<>(SB),NOSPLIT,$0-0
	JMP	kc_CFNumberCreate(SB)
GLOBL	·kc_CFNumberCreate_trampoline_addr(SB), RODATA, $8
DATA	·kc_CFNumberCreate_trampoline_addr(SB)/8, $kc_CFNumberCreate_trampoline<>(SB)

TEXT kc_CFPropertyListCreateWithData_trampoline<>(SB),NOSPLIT,$0-0
	JMP	kc_CFPropertyListCreateWithData(SB)
GLOBL	·kc_CFPropertyListCreateWithData_trampoline_addr(SB), RODATA, $8
DATA	·kc_CFPropertyListCreateWithData_trampoline_addr(SB)/8, $kc_CFPropertyListCreateWithData_trampoline<>(SB)

TEXT kc_CFStringCreateWithBytes_trampoline<>(SB),NOSPLIT,$0-0
	JMP	kc_CFStringCreateWithBytes(SB)
GLOBL	·kc_CFStringCreateWithBytes_trampoline_addr(SB), RODATA, $8
DATA	·kc_CFStringCreateWithBytes_trampoline_addr(SB)/8, $kc_CFStringCreateWithBytes_trampoline<>(SB)

TEXT kc_CFDictionaryCreateMutable_trampoline<>(SB),NOSPLIT,$0-0
	JMP	kc_CFDictionaryCreateMutable(SB)
GLOBL	·kc_CFDictionaryCreateMutable_trampoline_addr(SB), RODATA, $8
DATA	·kc_CFDictionaryCreateMutable_trampoline_addr(SB)/8, $kc_CFDictionaryCreateMutable_trampoline<>(SB)

TEXT kc_CFDictionarySetValue_trampoline<>(SB),NOSPLIT,$0-0
	JMP	kc_CFDictionarySetValue(SB)
GLOBL	·kc_CFDictionarySetValue_trampoline_addr(SB), RODATA, $8
DATA	·kc_CFDictionarySetValue_trampoline_addr(SB)/8, $kc_CFDictionarySetValue_trampoline<>(SB)

TEXT kc_CFArrayCreate_trampoline<>(SB),NOSPLIT,$0-0
	JMP	kc_CFArrayCreate(SB)
GLOBL	·kc_CFArrayCreate_trampoline_addr(SB), RODATA, $8
DATA	·kc_CFArrayCreate_trampoline_addr(SB)/8, $kc_CFArrayCreate_trampoline<>(SB)

TEXT kc_CFRelease_trampoline<>(SB),NOSPLIT,$0-0
	JMP	kc_CFRelease(SB)
GLOBL	·kc_CFRelease_trampoline_addr(SB), RODATA, $8
DATA	·kc_CFRelease_trampoline_addr(SB)/8, $kc_CFRelease_trampoline<>(SB)

TEXT kc_SecItemCopyMatching_trampoline<>(SB),NOSPLIT,$0-0
	JMP	kc_SecItemCopyMatching(SB)
GLOBL	·kc_SecItemCopyMatching_trampoline_addr(SB), RODATA, $8
DATA	·kc_SecItemCopyMatching_trampoline_addr(SB)/8, $kc_SecItemCopyMatching_trampoline<>(SB)

TEXT kc_SecItemAdd_trampoline<>(SB),NOSPLIT,$0-0
	JMP	kc_SecItemAdd(SB)
GLOBL	·kc_SecItemAdd_trampoline_addr(SB), RODATA, $8
DATA	·kc_SecItemAdd_trampoline_addr(SB)/8, $kc_SecItemAdd_trampoline<>(SB)

TEXT kc_SecItemUpdate_trampoline<>(SB),NOSPLIT,$0-0
	JMP	kc_SecItemUpdate(SB)
GLOBL	·kc_SecItemUpdate_trampoline_addr(SB), RODATA, $8
DATA	·kc_SecItemUpdate_trampoline_addr(SB)/8, $kc_SecItemUpdate_trampoline<>(SB)

TEXT kc_SecItemDelete_trampoline<>(SB),NOSPLIT,$0-0
	JMP	kc_SecItemDelete(SB)
GLOBL	·kc_SecItemDelete_trampoline_addr(SB), RODATA, $8
DATA	·kc_SecItemDelete_trampoline_addr(SB)/8, $kc_SecItemDelete_trampoline<>(SB)

TEXT kc_SecKeyCreateRandomKey_trampoline<>(SB),NOSPLIT,$0-0
	JMP	kc_SecKeyCreateRandomKey(SB)
GLOBL	·kc_SecKeyCreateRandomKey_trampoline_addr(SB), RODATA, $8
DATA	·kc_SecKeyCreateRandomKey_trampoline_addr(SB)/8, $kc_SecKeyCreateRandomKey_trampoline<>(SB)

TEXT kc_SecKeyCopyPublicKey_trampoline<>(SB),NOSPLIT,$0-0
	JMP	kc_SecKeyCopyPublicKey(SB)
GLOBL	·kc_SecKeyCopyPublicKey_trampoline_addr(SB), RODATA, $8
DATA	·kc_SecKeyCopyPublicKey_trampoline_addr(SB)/8, $kc_SecKeyCopyPublicKey_trampoline<>(SB)

TEXT kc_SecKeyCreateEncryptedData_trampoline<>(SB),NOSPLIT,$0-0
	JMP	kc_SecKeyCreateEncryptedData(SB)
GLOBL	·kc_SecKeyCreateEncryptedData_trampoline_addr(SB), RODATA, $8
DATA	·kc_SecKeyCreateEncryptedData_trampoline_addr(SB)/8, $kc_SecKeyCreateEncryptedData_trampoline<>(SB)

TEXT kc_SecKeyCreateDecryptedData_trampoline<>(SB),NOSPLIT,$0-0
	JMP	kc_SecKeyCreateDecryptedData(SB)
GLOBL	·kc_SecKeyCreateDecryptedData_trampoline_addr(SB), RODATA, $8
DATA	·kc_SecKeyCreateDecryptedData_trampoline_addr(SB)/8, $kc_SecKeyCreateDecryptedData_trampoline<>(SB)

TEXT kc_SecKeychainCreate_trampoline<>(SB),NOSPLIT,$0-0
	JMP	kc_SecKeychainCreate(SB)
GLOBL	·kc_SecKeychainCreate_trampoline_addr(SB), RODATA, $8
DATA	·kc_SecKeychainCreate_trampoline_addr(SB)/8, $kc_SecKeychainCreate_trampoline<>(SB)

TEXT kc_SecKeychainOpen_trampoline<>(SB),NOSPLIT,$0-0
	JMP	kc_SecKeychainOpen(SB)
GLOBL	·kc_SecKeychainOpen_trampoline_addr(SB), RODATA, $8
DATA	·kc_SecKeychainOpen_trampoline_addr(SB)/8, $kc_SecKeychainOpen_trampoline<>(SB)

TEXT kc_SecKeychainUnlock_trampoline<>(SB),NOSPLIT,$0-0
	JMP	kc_SecKeychainUnlock(SB)
GLOBL	·kc_SecKeychainUnlock_trampoline_addr(SB), RODATA, $8
DATA	·kc_SecKeychainUnlock_trampoline_addr(SB)/8, $kc_SecKeychainUnlock_trampoline<>(SB)

TEXT kc_SecKeychainDelete_trampoline<>(SB),NOSPLIT,$0-0
	JMP	kc_SecKeychainDelete(SB)
GLOBL	·kc_SecKeychainDelete_trampoline_addr(SB), RODATA, $8
DATA	·kc_SecKeychainDelete_trampoline_addr(SB)/8, $kc_SecKeychainDelete_trampoline<>(SB)
