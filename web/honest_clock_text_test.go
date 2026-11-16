package webui

import (
	"os"
	"strings"
	"testing"
)

// TestConsole_DoesNotPromiseAgeFading: under ACT-R (every preset) recall has
// no age term (honest clock, increment 1), so console copy that promises
// memories fade — or that a preset's temporal scoring is on/off — describes
// behaviour that does not exist. The phrases are the ones the console shipped.
func TestConsole_DoesNotPromiseAgeFading(t *testing.T) {
	for _, f := range []string{"templates/index.html", "static/js/app.js"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		lower := strings.ToLower(string(b))
		for _, claim := range []string{"fading", "temporal decay", "cognitive decay", "temporal scoring on", "temporal scoring off", "temporal scoring runs"} {
			if strings.Contains(lower, claim) {
				t.Errorf("%s still claims %q", f, claim)
			}
		}
	}
}
