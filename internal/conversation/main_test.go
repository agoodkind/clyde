package conversation_test

import (
	"os"
	"testing"

	"goodkind.io/clyde/internal/xdgtest"
)

// TestMain runs the package tests with Clyde state and cache directories under a temporary root.
func TestMain(m *testing.M) {
	os.Exit(xdgtest.Main(m))
}
