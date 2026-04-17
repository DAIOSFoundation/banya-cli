package client_test

import (
	"os"
	"testing"

	"github.com/cascadecodes/banya-cli/internal/client"
)

func TestSidecarPing(t *testing.T) {
	bin := os.Getenv("BANYA_CORE_BIN")
	if bin == "" {
		t.Skip("BANYA_CORE_BIN not set")
	}
	pc, err := client.NewProcessClient(bin)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	defer pc.Close()
	if err := pc.HealthCheck(); err != nil {
		t.Fatalf("health: %v", err)
	}
}
