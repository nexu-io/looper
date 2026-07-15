package api

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// httptest.NewRequest uses Host "example.com" for path-only URLs. Production
	// always validates the real Host; under tests, map that synthetic default to
	// the configured server authority so existing unit tests remain focused on
	// route behavior (see rewriteHTTPtestDefaultHost / effectiveRequestHost).
	rewriteHTTPtestDefaultHost = true
	os.Exit(m.Run())
}
