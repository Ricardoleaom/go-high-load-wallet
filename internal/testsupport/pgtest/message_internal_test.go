package pgtest

import (
	"errors"
	"strings"
	"testing"
)

func TestUnavailableErrorExplainsHowToFix(t *testing.T) {
	err := unavailableError(errors.New("docker socket not found"))

	for _, want := range []string{"DATABASE_URL", "Docker", "-short", "docker socket not found"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}
