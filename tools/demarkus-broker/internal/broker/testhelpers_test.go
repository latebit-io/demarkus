package broker

import "os"

// writeFile is a tiny helper for tests; keeps test imports clean of os
// boilerplate. The 0o600 mode matches what protocol/token writes elsewhere.
func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o600)
}
