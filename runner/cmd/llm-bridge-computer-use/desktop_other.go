//go:build !windows && !darwin

package main

func newDesktop() (desktop, error) {
	return nil, errDesktopUnsupported
}
