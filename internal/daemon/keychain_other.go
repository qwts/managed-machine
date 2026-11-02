//go:build !darwin

package daemon

func defaultKeyStore(string) keyStore { return nil }
