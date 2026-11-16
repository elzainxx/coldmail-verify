package api

// bulk_rows.go — the bulk request's per-row identity (owner ruling A1, research gap M1-G1) and the shape of a bulk
// result element. See Docs/bulk-item-identity.md for the contract this file implements.
//
//	Request: EXACTLY ONE of
//	  {"addresses": ["a@x.com", ...]}                              the original shape; its rows have no item_id
//	  {"items": [{"item_id": "r-1", "address": "a@x.com"}, ...]}   the same rows, each with the caller's own label
//	plus the unchanged "policy" and "caller_ref".
//
//	Result element: the Door-1 verdict object, byte for byte, plus ADDITIVE keys: "ordinal" (the row's 0-based
//	position in the submitted list, malformed rows counted), "item_id" (only when the row had one) and "duplicate_of"
//	(only on a row that repeats an earlier row's address: the ordinal of the FIRST such row).

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/coldmail-verify/control-plane/internal/queue"
	"github.com/coldmail-verify/control-plane/internal/store"
	"github.com/coldmail-verify/control-plane/internal/verdict"
)

// The two issue slugs this feature adds to the fixed field-issue vocabulary (errors.go). Additive-only, like the
// codes: never renamed, never repurposed, never free text.
const (
	issueConflict  = "conflict"  // two fields were given where exactly one may be
	issueDuplicate = "duplicate" // a value that must be unique within the request appears more than once
)

// maxValidationDetails bounds details[] on a validation error. A request with 100,000 duplicate item_ids must not
// be answered with a 100,000-entry error body; the first maxValidationDetails offending rows (lowest ordinal first)
// are named, which is what a caller needs to find and fix the pattern.
const maxValidationDetails = 100

// maxBulkBodyBytes bounds the request body: 100,000 rows (the submission cap) of a 200-byte item_id, a
// 254-byte address and JSON punctuation is about 50 MB, so the limit is set above that; it was 16 MiB when a row
// was a bare address and could not carry a label.
const maxBulkBodyBytes = 64 << 20

// bulkItemRequest is one row of the `items` request shape.
type bulkItemRequest struct {
	ItemID  string `json:"item_id"`
	Address string `json:"address"`
}

// bulkResultElement is one element of GET /v1/jobs/{id}/results: the Door-1 verdict object, embedded so every
// existing key is emitted exactly as before (Door 1: never removed, renamed or retyped), plus the row's identity.
// The embedded type has no custom marshaller, so its fields are flattened into this object's keys.
type bulkResultElement struct {
	verdict.Response
	// ItemID is the caller's label for the row, present only when the row had one.
	ItemID string `json:"item_id,omitempty"`
	// Ordinal is the row's 0-based position in the submitted list. ALWAYS present: 0 is a real position.
	Ordinal int `json:"ordinal"`
	// DuplicateOf is the ordinal of the FIRST row of the job with the same address, present only on a row that repeats
	// an earlier one (such rows share one check and one verdict; the platform charges the address once). Absent on a
	// first row and on every malformed row. A pointer so that a duplicate of row 0 says `0` rather than vanishing.
	DuplicateOf *int `json:"duplicate_of,omitempty"`
}

// malformedRowJSON is one entry of the create response's malformed_rows.
type malformedRowJSON struct {
	Ordinal int    `json:"ordinal"`
	ItemID  string `json:"item_id,omitempty"`
}

// bulkShapeErrors judges which of the two request shapes was given. Exactly one may be; both or neither is a 400
// naming the fields. "Given" means the key was present with a JSON array (null or absent is not given), so an
// empty array is a given-but-empty field and is refused as `required`, as an empty `addresses` always was.
// A nil result means the shape is acceptable.
func bulkShapeErrors(req bulkRequest) []fieldDetail {
	addressesGiven, itemsGiven := req.Addresses != nil, req.Items != nil
	switch {
	case addressesGiven && itemsGiven:
		return []fieldDetail{{Field: "addresses", Issue: issueConflict}, {Field: "items", Issue: issueConflict}}
	case !addressesGiven && !itemsGiven:
		return []fieldDetail{{Field: "addresses", Issue: issueRequired}, {Field: "items", Issue: issueRequired}}
	case addressesGiven && len(req.Addresses) == 0:
		return []fieldDetail{{Field: "addresses", Issue: issueRequired}}
	case itemsGiven && len(req.Items) == 0:
		return []fieldDetail{{Field: "items", Issue: issueRequired}}
	}
	return nil
}

// validateItemIDs applies the item_id rules to the `items` shape and returns the offending rows, lowest ordinal
// first and at most maxValidationDetails of them. A rule never rejects an ADDRESS: a bad address is a malformed
// row (it keeps its ordinal and is reported), not a bad request. The rules, per ruling A1:
//   - non-blank (a missing key, "" and whitespace only all count as blank) -> required
//   - at most store.MaxItemIDBytes bytes                                    -> out_of_range
//   - no control characters (unicode category Cc: NUL, newline, tab, DEL...) -> malformed
//   - unique within the request, compared exactly (case-sensitive, bytes)    -> duplicate, naming EVERY row that
//     shares the value (the earlier ones as well as the later), so the caller sees the whole group.
func validateItemIDs(items []bulkItemRequest) []fieldDetail {
	type bad struct {
		ordinal int
		issue   string
	}
	var found []bad
	firstOrdinal := make(map[string]int, len(items))
	dupGroups := map[string][]int{} // item_id -> every ordinal that carries it, once two or more do
	for i, it := range items {
		switch {
		case strings.TrimSpace(it.ItemID) == "":
			found = append(found, bad{i, issueRequired})
		case len(it.ItemID) > store.MaxItemIDBytes:
			found = append(found, bad{i, issueOutOfRange})
		case strings.IndexFunc(it.ItemID, unicode.IsControl) >= 0:
			found = append(found, bad{i, issueMalformed})
		default:
			if first, seen := firstOrdinal[it.ItemID]; seen {
				if _, started := dupGroups[it.ItemID]; !started {
					dupGroups[it.ItemID] = []int{first}
				}
				dupGroups[it.ItemID] = append(dupGroups[it.ItemID], i)
			} else {
				firstOrdinal[it.ItemID] = i
			}
		}
	}
	for _, ordinals := range dupGroups {
		for _, o := range ordinals {
			found = append(found, bad{o, issueDuplicate})
		}
	}
	if len(found) == 0 {
		return nil
	}
	sort.Slice(found, func(a, b int) bool { return found[a].ordinal < found[b].ordinal })
	if len(found) > maxValidationDetails {
		found = found[:maxValidationDetails]
	}
	details := make([]fieldDetail, len(found))
	for i, f := range found {
		details[i] = fieldDetail{Field: fmt.Sprintf("items[%d].item_id", f.ordinal), Issue: f.issue}
	}
	return details
}

// bulkRows turns a request that passed the shape check into the enqueuer's rows.
func bulkRows(req bulkRequest) []queue.BulkRow {
	if req.Items != nil {
		rows := make([]queue.BulkRow, len(req.Items))
		for i, it := range req.Items {
			rows[i] = queue.BulkRow{ItemID: it.ItemID, Address: it.Address}
		}
		return rows
	}
	rows := make([]queue.BulkRow, len(req.Addresses))
	for i, a := range req.Addresses {
		rows[i] = queue.BulkRow{Address: a}
	}
	return rows
}
