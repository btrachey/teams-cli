package doctor

import (
	"strings"
	"testing"
	"time"

	"github.com/fossteams/teams-cli/internal/config"
)

func TestDoctorTokenRefreshCheckStates(t *testing.T) {
	disabled := doctorTokenRefreshCheck(config.Options{AutoRefresh: false})
	if disabled.Status != doctorWarn || !strings.Contains(disabled.Details, "disabled") {
		t.Fatalf("expected disabled warning, got %+v", disabled)
	}

	custom := doctorTokenRefreshCheck(config.Options{
		AutoRefresh:         true,
		TokenRefreshCommand: "my-token-tool",
		TokenRefreshTimeout: time.Minute,
	})
	if custom.Status != doctorPass || !strings.Contains(custom.Details, "my-token-tool") {
		t.Fatalf("expected custom command pass, got %+v", custom)
	}
}
