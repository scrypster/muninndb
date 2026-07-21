package webui

import (
	"strings"
	"testing"
)

func TestAdminWorkerHelpMatchesRuntimeArchitecture(t *testing.T) {
	page, err := FS.ReadFile("templates/index.html")
	if err != nil {
		t.Fatal(err)
	}
	body := string(page)

	if strings.Contains(body, "Configured via server startup flags") {
		t.Fatal("admin help still claims workers are configured by startup flags")
	}
	if !strings.Contains(body, "Temporal scoring runs at query time") {
		t.Fatal("admin help does not explain query-time temporal scoring")
	}
	if !strings.Contains(body, "Dormant workers are enabled") {
		t.Fatal("admin help does not explain dormant worker wake-up behavior")
	}
}
