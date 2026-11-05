package main

import (
	"os/exec"
	"testing"
)

// Runs the real app.js with a fake page: what the human is told when things go
// wrong. Browsers are tested on the VM.
func TestViewerPage(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	out, err := exec.Command(node, "testdata/app_test.mjs", "web/app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
