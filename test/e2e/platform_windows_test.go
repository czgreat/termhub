//go:build windows

package e2e

import (
	"fmt"
	"testing"
	"time"
)

const exeSuffix = ".exe"

func testPipe(t *testing.T) string {
	return fmt.Sprintf(`\\.\pipe\termhub-e2e-%d`, time.Now().UnixNano())
}
