package hash

import "strings"

// SplitNormalized splits a norm/v1 address (the first value Hasher.Normalize returns) into its local part and its domain, at the LAST '@'.
//
// It is THE split the rest of the module uses to turn a normalised address into the two parts the cheap layers judge: the orchestrator calls it to
// build the layers' Evidence (internal/orchestrator), and the shared vector file's `syntax_ok` expectations are produced and checked through it
// (norm_vectors_test.go), so the file and the live request path cannot disagree about where the local part ends. It was an inline expression in the
// orchestrator (`normalized[:indexAt(normalized)]`) before the shared file gained syntax cases; extracting it changed no behaviour.
//
// A normalised address always holds exactly one '@' (Normalize refuses everything else), so both results are non-empty for every address that
// reaches a layer. Given a string with no '@' (which Normalize never returns) both results are empty, exactly what the old inline expression gave for
// the local part. The domain half equals the second value Normalize returns; norm_vectors_test.go asserts that on every vector of the shared file.
func SplitNormalized(normalized string) (localPart, domain string) {
	at := strings.LastIndex(normalized, "@")
	if at < 0 {
		return "", ""
	}
	return normalized[:at], normalized[at+1:]
}
