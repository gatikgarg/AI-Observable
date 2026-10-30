//go:build windows

package integration

import (
	"fmt"
	"os"
	"testing"
)

func TestWindowsUserEnvironmentRoundTrip(t *testing.T) {
	name := fmt.Sprintf("OBSERVABLE_TEST_THROWAWAY_%d", os.Getpid())
	if err := setUserEnv(name, "throwaway-not-a-secret"); err != nil {
		t.Skipf("registry access unavailable in this sandbox: %v", err)
	}
	t.Cleanup(func() { _ = unsetUserEnv(name) })
	got, err := getUserEnv(name)
	if err != nil {
		t.Fatal(err)
	}
	if got != "throwaway-not-a-secret" {
		t.Fatalf("persisted value = %q", got)
	}
}
