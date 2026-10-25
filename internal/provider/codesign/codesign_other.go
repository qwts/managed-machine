//go:build !darwin

package codesign

import "github.com/qwts/managed-machine/internal/provider"

func defaultRunner() (provider.Runner, error) {
	return nil, provider.Kind(provider.ErrUnsupported, "codesign verification is supported only on darwin/arm64")
}
