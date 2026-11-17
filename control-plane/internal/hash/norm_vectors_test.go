package hash

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// The SHARED normalisation vector file. testdata/norm-vectors.json is a BYTE-IDENTICAL copy of the file the
// platform keeps at internal/norm/testdata/norm-vectors.json (repository coldmail-platform); the platform's
// own normaliser and this module's Hasher.Normalize must give the same answer on every vector, and the
// platform's harness fails if the two copies differ. The expectations were produced by running THIS
// package's real Hasher.Normalize, so this test is the reference side: it reads the file and runs every
// vector through the real function, nothing is mocked. A change to the file is a change to a FROZEN scheme
// (Door 3, norm/v1): make it in both repositories at once.
//
// What is asserted per vector: malformed == (Normalize returned an error), using the predicate
// internal/queue/enqueue.go uses to decide a bulk row is malformed (`nerr != nil`), and that the error is
// ErrUnparseable; the normalized string byte for byte; the returned domain is the part after the '@'.
// What is asserted across vectors: every member of a group normalises to one string, different groups to
// different strings. The checker is a pure function over a table so that the paired control can run it on
// a doctored copy of the table and prove it reports each kind of difference by the vector's name.

const (
	normVectorsPath   = "testdata/norm-vectors.json"
	normVectorsFormat = "coldmail-norm-vectors/1"
	normVectorsScheme = "norm/v1"
	normVectorsFloor  = 200 // the file holds 207; an empty or truncated file must be red, not vacuously green
)

type normVector struct {
	Name       string  `json:"name"`
	Input      *string `json:"input"`
	InputB64   *string `json:"input_b64"`
	Malformed  bool    `json:"malformed"`
	Normalized *string `json:"normalized"`
	Group      string  `json:"group"`
	Why        string  `json:"why"`
}

type normVectorFile struct {
	Format  string       `json:"format"`
	Scheme  string       `json:"scheme"`
	Vectors []normVector `json:"vectors"`
}

func loadNormVectors(t *testing.T) normVectorFile {
	t.Helper()
	b, err := os.ReadFile(normVectorsPath)
	if err != nil {
		t.Fatalf("read %s: %v", normVectorsPath, err)
	}
	var f normVectorFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("decode %s: %v", normVectorsPath, err)
	}
	return f
}

// rawInput returns the exact bytes the vector feeds to Normalize.
func (v normVector) rawInput() (string, error) {
	switch {
	case v.Input != nil && v.InputB64 != nil:
		return "", errors.New("both input and input_b64 are present")
	case v.Input != nil:
		return *v.Input, nil
	case v.InputB64 != nil:
		b, err := base64.StdEncoding.DecodeString(*v.InputB64)
		if err != nil {
			return "", fmt.Errorf("input_b64 is not base64: %v", err)
		}
		return string(b), nil
	}
	return "", errors.New("neither input nor input_b64 is present")
}

func clonePtr(p *string) *string {
	if p == nil {
		return nil
	}
	s := *p
	return &s
}

func cloneVectors(vs []normVector) []normVector {
	out := make([]normVector, len(vs))
	for i, v := range vs {
		v.Input, v.InputB64, v.Normalized = clonePtr(v.Input), clonePtr(v.InputB64), clonePtr(v.Normalized)
		out[i] = v
	}
	return out
}

// checkNormVectors runs every vector through h.Normalize and returns one message per difference; each message
// names the vector (or the group). An empty result means the table and the function agree.
func checkNormVectors(h *Hasher, vs []normVector) []string {
	var problems []string
	add := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }

	if len(vs) < normVectorsFloor {
		add("floor: %d vectors, want at least %d (an empty or truncated file must not read as green)", len(vs), normVectorsFloor)
	}
	seen := map[string]bool{}
	actualByGroup := map[string]string{} // group -> the string the REAL function gave its first member
	groupOfActual := map[string]string{} // that string -> the group that owns it
	accepted, rejected, grouped := 0, 0, 0
	for _, v := range vs {
		if v.Name == "" {
			add("a vector has no name (why: %q)", v.Why)
			continue
		}
		if seen[v.Name] {
			add("%s: duplicate vector name", v.Name)
		}
		seen[v.Name] = true

		raw, err := v.rawInput()
		if err != nil {
			add("%s: %v", v.Name, err)
			continue
		}
		email, domain, nerr := h.Normalize(raw)
		isMalformed := nerr != nil // the predicate internal/queue/enqueue.go uses for a malformed bulk row
		if isMalformed != v.Malformed {
			add("%s: malformed = %v in the file, Normalize gave error=%v (%q -> %q)", v.Name, v.Malformed, nerr, raw, email)
			continue
		}
		if isMalformed {
			rejected++
			if !errors.Is(nerr, ErrUnparseable) {
				add("%s: a malformed vector must fail with ErrUnparseable, got %v", v.Name, nerr)
			}
			if email != "" || domain != "" {
				add("%s: a refused address returned a value (%q, %q): it must return nothing", v.Name, email, domain)
			}
			if v.Normalized != nil || v.Group != "" {
				add("%s: a malformed vector must carry neither normalized nor group", v.Name)
			}
			continue
		}
		accepted++
		if v.Normalized == nil {
			add("%s: an accepted vector has no normalized value", v.Name)
			continue
		}
		if email != *v.Normalized {
			add("%s: normalized differs: Normalize(%q) = %q, the file says %q", v.Name, raw, email, *v.Normalized)
		}
		if at := strings.LastIndex(email, "@"); at < 0 || domain != email[at+1:] {
			add("%s: the returned domain %q is not the part of %q after the '@'", v.Name, domain, email)
		}
		if v.Group == "" {
			add("%s: an accepted vector has no group", v.Name)
			continue
		}
		grouped++
		if first, ok := actualByGroup[v.Group]; !ok {
			actualByGroup[v.Group] = email
		} else if first != email {
			add("group %q is inconsistent: %s normalises to %q but an earlier member gave %q", v.Group, v.Name, email, first)
		}
		if owner, ok := groupOfActual[email]; !ok {
			groupOfActual[email] = v.Group
		} else if owner != v.Group {
			add("groups %q and %q both normalise to %q (%s): different groups must differ", owner, v.Group, email, v.Name)
		}
	}
	// A non-vacuous table: both outcomes are present, and groups really group (some group has several members).
	if accepted == 0 || rejected == 0 {
		add("the table is one-sided: %d accepted and %d malformed vectors", accepted, rejected)
	}
	if len(actualByGroup) == 0 || grouped <= len(actualByGroup) {
		add("no group has more than one member (%d accepted vectors in %d groups): the group check would be vacuous", grouped, len(actualByGroup))
	}
	return problems
}

func hasProblem(problems []string, subs ...string) bool {
	for _, p := range problems {
		ok := true
		for _, s := range subs {
			if !strings.Contains(p, s) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// EVERY vector of the shared file through the real Hasher.Normalize: the malformed flag, the normalized
// string byte for byte, and the group structure. On any difference the message names the vector.
func TestNormVectors_SharedFile_EveryVectorThroughNormalize(t *testing.T) {
	f := loadNormVectors(t)
	if f.Format != normVectorsFormat || f.Scheme != normVectorsScheme {
		t.Fatalf("the vector file says format %q scheme %q, want %q / %q", f.Format, f.Scheme, normVectorsFormat, normVectorsScheme)
	}
	for _, p := range checkNormVectors(newTestHasher(t), f.Vectors) {
		t.Error(p)
	}
	if len(f.Vectors) < normVectorsFloor {
		t.Fatalf("only %d vectors: the shared file is truncated (floor %d)", len(f.Vectors), normVectorsFloor)
	}
}

// PAIRED CONTROL. The checker is run on doctored copies of the real table, in memory, and must report each
// kind of difference by the vector's name (or group). The undoctored table must be clean, so a red here is
// the doctoring and not a broken table; and a checker that accepted everything would fail every case below.
func TestNormVectors_Checker_ReportsADoctoredTable(t *testing.T) {
	h := newTestHasher(t)
	f := loadNormVectors(t)
	if p := checkNormVectors(h, f.Vectors); len(p) != 0 {
		t.Fatalf("control setup: the real table must be clean, got %v", p)
	}

	// Find vectors to doctor, by property, not by position.
	idx := func(pred func(v normVector) bool) int {
		for i, v := range f.Vectors {
			if pred(v) {
				return i
			}
		}
		t.Fatal("control setup: no vector with the wanted property")
		return -1
	}
	accepted := idx(func(v normVector) bool { return !v.Malformed && v.Input != nil })
	malformed := idx(func(v normVector) bool { return v.Malformed })
	b64 := idx(func(v normVector) bool { return v.InputB64 != nil })
	// Two accepted vectors of DIFFERENT groups (gA first in file order, gB the first later vector of another
	// group, so gB is the first member of its group), and two members of ONE group (sameA, then sameB later).
	// Chosen in file order, never by map iteration, so a doctored message always names the later vector.
	gA := idx(func(v normVector) bool { return !v.Malformed })
	gB := -1
	for i := gA + 1; i < len(f.Vectors); i++ {
		if v := f.Vectors[i]; !v.Malformed && v.Group != f.Vectors[gA].Group {
			gB = i
			break
		}
	}
	sameA, sameB := -1, -1
	firstOf := map[string]int{}
	for i, v := range f.Vectors {
		if v.Malformed {
			continue
		}
		if j, ok := firstOf[v.Group]; ok {
			sameA, sameB = j, i
			break
		}
		firstOf[v.Group] = i
	}
	if gB < 0 || sameB < 0 || sameA == gA && sameB == gB {
		t.Fatal("control setup: the table has no multi-member group or no two groups")
	}

	str := func(s string) *string { return &s }
	cases := []struct {
		name   string
		doctor func(vs []normVector) (changed string) // returns the name of the vector or group the report must mention
		want   string                                 // a fragment of the expected message
	}{
		{"an expected normalized value is changed", func(vs []normVector) string {
			vs[accepted].Normalized = str(*vs[accepted].Normalized + "x")
			return vs[accepted].Name
		}, "normalized differs"},
		{"an accepted vector is flagged malformed", func(vs []normVector) string {
			vs[accepted].Malformed = true
			return vs[accepted].Name
		}, "malformed = true in the file"},
		{"a malformed vector is flagged accepted", func(vs []normVector) string {
			vs[malformed].Malformed = false
			vs[malformed].Normalized = str("x@example.com")
			vs[malformed].Group = "doctored"
			return vs[malformed].Name
		}, "malformed = false in the file"},
		{"a malformed vector carries a normalized value", func(vs []normVector) string {
			vs[malformed].Normalized = str("x@example.com")
			return vs[malformed].Name
		}, "neither normalized nor group"},
		{"a member of a group is given another group's string", func(vs []normVector) string {
			vs[sameB].Normalized = str(*vs[gB].Normalized)
			return vs[sameB].Name
		}, "normalized differs"},
		{"a vector is moved into another group", func(vs []normVector) string {
			vs[gB].Group = vs[gA].Group
			return vs[gB].Name
		}, "is inconsistent"},
		{"the input of a vector is changed so it no longer gives its expectation", func(vs []normVector) string {
			vs[accepted].Input = str("different." + *vs[accepted].Input)
			return vs[accepted].Name
		}, "normalized differs"},
		{"a base64 input is corrupted", func(vs []normVector) string {
			vs[b64].InputB64 = str("!!not base64!!")
			return vs[b64].Name
		}, "is not base64"},
		{"a vector has both input and input_b64", func(vs []normVector) string {
			vs[accepted].InputB64 = str("YUBiLmNvbQ==")
			return vs[accepted].Name
		}, "both input and input_b64"},
		{"a vector has neither input nor input_b64", func(vs []normVector) string {
			vs[accepted].Input = nil
			return vs[accepted].Name
		}, "neither input nor input_b64"},
		{"two vectors share a name", func(vs []normVector) string {
			vs[gB].Name = vs[gA].Name
			return vs[gA].Name
		}, "duplicate vector name"},
		{"an accepted vector loses its group", func(vs []normVector) string {
			vs[accepted].Group = ""
			return vs[accepted].Name
		}, "has no group"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			vs := cloneVectors(f.Vectors)
			subject := c.doctor(vs)
			if problems := checkNormVectors(h, vs); !hasProblem(problems, subject, c.want) {
				t.Errorf("the checker did not report %q mentioning %q; it reported: %v", c.want, subject, problems)
			}
		})
	}

	// A file truncated below the floor is red, whatever it still contains.
	t.Run("the file is truncated below the floor", func(t *testing.T) {
		vs := cloneVectors(f.Vectors)[:normVectorsFloor-1]
		if problems := checkNormVectors(h, vs); !hasProblem(problems, "floor:") {
			t.Errorf("%d vectors were not reported as below the floor: %v", len(vs), problems)
		}
	})

	// Two groups that collide on one string, with the expectations CORRECT for the doctored inputs: the
	// cross-group rule fires on its own. (Same input in two groups: the function gives one string for both.)
	t.Run("two groups whose inputs normalise to one string", func(t *testing.T) {
		vs := cloneVectors(f.Vectors)
		vs[gB].Input = clonePtr(vs[gA].Input)
		vs[gB].InputB64 = clonePtr(vs[gA].InputB64)
		vs[gB].Normalized = clonePtr(vs[gA].Normalized)
		problems := checkNormVectors(h, vs)
		if !hasProblem(problems, "different groups must differ") {
			t.Errorf("two groups collided on one normalised string and the checker did not say so: %v", problems)
		}
	})

	// Empty and nil tables are red, not vacuously green.
	for name, vs := range map[string][]normVector{"empty": {}, "nil": nil} {
		if p := checkNormVectors(h, vs); !hasProblem(p, "floor:") {
			t.Errorf("%s table: the floor was not reported: %v", name, p)
		}
	}

	// The control is not a constant: with the real table the same checker is silent (a checker that always
	// reported something would pass every case above for the wrong reason).
	if p := checkNormVectors(h, cloneVectors(f.Vectors)); len(p) != 0 {
		t.Errorf("the undoctored copy reported %v", p)
	}
}
