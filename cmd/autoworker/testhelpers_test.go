package main

import "os"

func writeFileForTest(path string) error {
	return os.WriteFile(path, []byte("x"), 0o600)
}
