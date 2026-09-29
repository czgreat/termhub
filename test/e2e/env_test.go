package e2e

import (
	"os"
	"strings"
)

func testEnvironment() []string {
	var out []string
	for _, v := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(v), "TH_") {
			out = append(out, v)
		}
	}
	return out
}
