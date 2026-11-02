package main

import (
	"fmt"
)

// The darwin archive carries the CLI twice: a bare managed-machine for
// install-core.sh, and the same binary as the main executable of an app-like
// bundle. Only the bundle can hold the Developer ID provisioning profile that
// lets the daemon use its keychain access group and the Secure Enclave, so the
// formula links bin/managed-machine into the bundle.
const (
	bundleName       = "AgentBotMachine.app"
	bundleID         = "ai.true-line.agent-bot-machine"
	bundleExecutable = "managed-machine"
	bundleInfoPlist  = bundleName + "/Contents/Info.plist"
	bundleMainExe    = bundleName + "/Contents/MacOS/" + bundleExecutable
	bundleProfile    = bundleName + "/Contents/embedded.provisionprofile"
	bundleSealed     = bundleName + "/Contents/_CodeSignature/CodeResources"
	bundleTicket     = bundleName + "/Contents/CodeResources"
	minimumMacOS     = "15.0"
)

// signedOnlyEntries are the files codesign, the profile copy and stapler add
// to the bundle. A signed archive may contain these and nothing else that the
// unsigned archive lacks.
var signedOnlyEntries = map[string]bool{
	bundleProfile: true,
	bundleSealed:  true,
	bundleTicket:  true,
}

func infoPlist(version string) []byte {
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleDevelopmentRegion</key>
	<string>en</string>
	<key>CFBundleExecutable</key>
	<string>%s</string>
	<key>CFBundleIdentifier</key>
	<string>%s</string>
	<key>CFBundleInfoDictionaryVersion</key>
	<string>6.0</string>
	<key>CFBundleName</key>
	<string>Agent Bot Machine</string>
	<key>CFBundlePackageType</key>
	<string>APPL</string>
	<key>CFBundleShortVersionString</key>
	<string>%s</string>
	<key>CFBundleVersion</key>
	<string>%s</string>
	<key>LSMinimumSystemVersion</key>
	<string>%s</string>
	<key>LSUIElement</key>
	<true/>
</dict>
</plist>
`, bundleExecutable, bundleID, version, version, minimumMacOS))
}

func darwinBundleEntries(version string, binary []byte) []archiveEntry {
	return []archiveEntry{
		{Name: bundleInfoPlist, Mode: 0o644, Data: infoPlist(version)},
		{Name: bundleMainExe, Mode: 0o755, Data: binary},
	}
}
