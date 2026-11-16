package main

// M1-G7: TM_QUEUE_ERROR_MAX_ATTEMPTS is a dial, and "every dial needs a consumer and a doc" is a gate, not a note. The CONSUMER is
// buildQueue handing the configured value to the drainer (internal/queue's own tests prove what the drainer does with it); the DOCS are
// the config manifest (authoritative) and the generated reference. This pins both ends: delete the wiring line and the dial silently falls
// back to the drainer's built-in default while every config test stays green — an operator could set it and nothing would happen, the
// settable-but-ignored footgun this repository has removed before (TM_AUDIT_INTERVAL_SECONDS, TM_EXTERNAL_CONFIDENCE_CAP).

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestErrorCapDial_HasAConsumerAndADoc(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if !regexp.MustCompile(`ErrorMaxAttempts:\s*cfg\.Dials\.QueueErrorMaxAttempts`).Match(src) {
		t.Error("buildQueue no longer passes cfg.Dials.QueueErrorMaxAttempts to the drainer (queue.Deps.ErrorMaxAttempts): the dial has no consumer")
	}

	for _, doc := range []string{"../../../Docs/config-manifest.md", "../../../Docs/config-reference.md"} {
		b, err := os.ReadFile(doc)
		if err != nil {
			t.Fatalf("read %s: %v", doc, err)
		}
		if !strings.Contains(string(b), "TM_QUEUE_ERROR_MAX_ATTEMPTS") {
			t.Errorf("%s does not document TM_QUEUE_ERROR_MAX_ATTEMPTS (regenerate the reference with scripts/gen-config-reference.sh after editing the manifest)", doc)
		}
	}
}
