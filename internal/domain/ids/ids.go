// Package ids defines what the service accepts as an entity identifier.
package ids

import "github.com/google/uuid"

// Valid reports whether s is a UUID in its canonical 36-character form,
// the only form the service hands out and the only one it accepts back.
func Valid(s string) bool {
	return len(s) == 36 && uuid.Validate(s) == nil
}
