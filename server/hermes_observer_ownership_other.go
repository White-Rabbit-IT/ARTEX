//go:build !unix

package server

import (
	"fmt"
	"os"
)

// The observer's privilege boundary is documented and supported on Unix/Docker
// hosts. Refuse rather than silently weaken owner validation on another OS.
func validateHermesObserverOwnership(root, pending os.FileInfo) error {
	return fmt.Errorf("hermes observer requires Unix ownership validation")
}
