package hash

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/coldmail-verify/control-plane/internal/layers"
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
//
// FORMAT /2 ADDS THE SYNTAX RULE (owner ruling OD-9: the platform pre-validates with norm/v1 plus module 1's syntax layer, exactly as module
// 1 implements it). Every non-malformed vector carries `syntax_ok`, the answer of the REAL layers.SyntaxLayer (run through
// layers.NewEvidence and Run, Result == Pass) over the parts SplitNormalized returns for the normalised address, which is the split the
// orchestrator uses; a malformed vector never reaches the layer and carries none. The `syntax-*` vectors pin every boundary of the rule on
// both sides (64 and 65 bytes of local part, ASCII and multi-byte; 254, 255 and 256 bytes of domain; a dot or no dot, including a dot that
// normalisation strips; a punycode form longer than its Unicode form). The checker takes the syntax rule as a parameter so the paired
// control can run it with a one-character mutant of the rule and prove a boundary vector reports it by name.

const (
	normVectorsPath   = "testdata/norm-vectors.json"
	normVectorsFormat = "coldmail-norm-vectors/2"
	normVectorsScheme = "norm/v1"
	normVectorsFloor  = 230 // the file holds 238; an empty or truncated file must be red, not vacuously green

	// The syntax_ok outcomes the file must hold on BOTH sides: a table of only true (or only false) pins nothing.
	syntaxTrueFloor  = 100 // the file holds 159
	syntaxFalseFloor = 15  // the file holds 24
)

// requiredSyntaxBoundaries are the boundary vectors the file MUST contain, with the answer each must carry: deleting one, or editing its
// syntax_ok together with the layer, would let a change of the rule through, so the names and answers are pinned here, not only in the file.
var requiredSyntaxBoundaries = map[string]bool{
	"syntax-local-64-ascii":                            true,
	"syntax-local-65-ascii":                            false,
	"syntax-local-64-bytes-two-byte-runes":             true,
	"syntax-local-66-bytes-33-runes":                   false, // 33 runes but 66 bytes: the limit is in bytes
	"syntax-local-64-bytes-from-decomposed-input":      true,  // NFC shortens the raw 96 bytes to 64: the rule judges the normalised part
	"syntax-local-lowercase-shrinks-to-64-bytes":       true,
	"syntax-domain-254-ascii":                          true,
	"syntax-domain-255-ascii":                          true,
	"syntax-domain-256-ascii":                          false,
	"syntax-domain-255-after-trailing-dot":             true, // 256 raw bytes, 255 once the trailing dot is stripped
	"syntax-domain-256-after-trailing-dot":             false,
	"syntax-idn-ascii-form-at-or-under-255":            true,
	"syntax-idn-ascii-form-over-255":                   false, // the punycode form is longer than the Unicode form and over 255
	"syntax-domain-one-dot-minimum":                    true,
	"syntax-domain-no-dot":                             false,
	"syntax-domain-only-a-trailing-dot":                false, // 'b.' has a dot in the raw input and none after normalisation
	"syntax-domain-localhost":                          false,
	"syntax-domain-leading-dot":                        true,
	"syntax-domain-double-dot":                         true,
	"syntax-domain-several-trailing-dots-leave-no-dot": false,
	"syntax-local-plus-tag":                            true,
}

// syntaxRule is a syntax judgement over the two normalised parts. The real one runs the shipped layer.
type syntaxRule func(localPart, domain string) bool

// realSyntaxRule runs module 1's SyntaxLayer the way the orchestrator builds its evidence (layers.NewEvidence over the normalised address and
// the two parts of the split) and reads the verdict the way the policy engine does: Result == Pass.
func realSyntaxRule(localPart, domain string) bool {
	ev := layers.NewEvidence("", localPart+"@"+domain, localPart, domain, nil, nil)
	return layers.SyntaxLayer{}.Run(context.Background(), ev).Result == layers.Pass
}

type normVector struct {
	Name       string  `json:"name"`
	Input      *string `json:"input"`
	InputB64   *string `json:"input_b64"`
	Malformed  bool    `json:"malformed"`
	Normalized *string `json:"normalized"`
	SyntaxOK   *bool   `json:"syntax_ok"` // format /2: present on every non-malformed vector, absent on a malformed one
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
		if v.SyntaxOK != nil {
			b := *v.SyntaxOK
			v.SyntaxOK = &b
		}
		out[i] = v
	}
	return out
}

// checkNormVectors runs every vector through h.Normalize and the real syntax layer and returns one message per difference; each message
// names the vector (or the group). An empty result means the table and the functions agree.
func checkNormVectors(h *Hasher, vs []normVector) []string {
	return checkNormVectorsWith(h, vs, realSyntaxRule)
}

// checkNormVectorsWith is checkNormVectors with the syntax rule as a parameter, so the paired control can run it with a mutant of the rule.
func checkNormVectorsWith(h *Hasher, vs []normVector, syntax syntaxRule) []string {
	var problems []string
	add := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }

	if len(vs) < normVectorsFloor {
		add("floor: %d vectors, want at least %d (an empty or truncated file must not read as green)", len(vs), normVectorsFloor)
	}
	seen := map[string]bool{}
	actualByGroup := map[string]string{} // group -> the string the REAL function gave its first member
	groupOfActual := map[string]string{} // that string -> the group that owns it
	accepted, rejected, grouped := 0, 0, 0
	syntaxTrue, syntaxFalse := 0, 0
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
			if v.SyntaxOK != nil {
				add("%s: a malformed vector must carry no syntax_ok (it never reaches the syntax layer)", v.Name)
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
		// The syntax rule, on the NORMALISED parts, split exactly as the orchestrator splits them.
		localPart, splitDomain := SplitNormalized(email)
		if splitDomain != domain {
			add("%s: SplitNormalized gives the domain %q, Normalize gave %q: the orchestrator's split and Normalize disagree", v.Name, splitDomain, domain)
		}
		realOK := syntax(localPart, domain)
		switch {
		case v.SyntaxOK == nil:
			add("%s: an accepted vector has no syntax_ok (format /2 requires it on every non-malformed vector)", v.Name)
		case *v.SyntaxOK != realOK:
			add("%s: syntax_ok = %v in the file, the syntax rule says %v (local part %d bytes, domain %d bytes)", v.Name, *v.SyntaxOK, realOK, len(localPart), len(domain))
		}
		if realOK {
			syntaxTrue++
		} else {
			syntaxFalse++
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
	// The boundary vectors are pinned by name, with the answer each must carry (the file's own claim is compared above; this stops a boundary
	// vector being deleted, or its expectation and the rule being changed together).
	byName := map[string]normVector{}
	for _, v := range vs {
		byName[v.Name] = v
	}
	for name, want := range requiredSyntaxBoundaries {
		v, ok := byName[name]
		switch {
		case !ok:
			add("required boundary vector %s is missing from the file", name)
		case v.Malformed || v.SyntaxOK == nil:
			add("required boundary vector %s must be an accepted vector with a syntax_ok", name)
		case *v.SyntaxOK != want:
			add("required boundary vector %s must carry syntax_ok = %v, the file says %v", name, want, *v.SyntaxOK)
		}
	}
	if syntaxTrue < syntaxTrueFloor || syntaxFalse < syntaxFalseFloor {
		add("syntax floor: %d accepted vectors pass the syntax rule and %d fail it, want at least %d and %d (a one-sided table pins nothing)", syntaxTrue, syntaxFalse, syntaxTrueFloor, syntaxFalseFloor)
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
	named := func(name string) int {
		return idx(func(v normVector) bool { return v.Name == name })
	}
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
		// FORMAT /2: each kind of syntax_ok difference is reported by the vector's name.
		{"the syntax_ok of a vector is flipped", func(vs []normVector) string {
			b := !*vs[accepted].SyntaxOK
			vs[accepted].SyntaxOK = &b
			return vs[accepted].Name
		}, "syntax_ok = "},
		{"a boundary vector's syntax_ok is flipped (one expectation, one boolean)", func(vs []normVector) string {
			i := named("syntax-local-65-ascii")
			b := !*vs[i].SyntaxOK
			vs[i].SyntaxOK = &b
			return vs[i].Name
		}, "syntax_ok = "},
		{"an accepted vector loses its syntax_ok", func(vs []normVector) string {
			vs[accepted].SyntaxOK = nil
			return vs[accepted].Name
		}, "has no syntax_ok"},
		{"a malformed vector gains a syntax_ok", func(vs []normVector) string {
			b := true
			vs[malformed].SyntaxOK = &b
			return vs[malformed].Name
		}, "must carry no syntax_ok"},
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

	// A required boundary vector that is deleted from the file is reported by name, and so is one whose syntax_ok was edited to agree with a
	// changed rule (the pinned answer is in this test, not only in the file).
	t.Run("a required boundary vector is deleted", func(t *testing.T) {
		vs := cloneVectors(f.Vectors)
		i := named("syntax-domain-256-ascii")
		vs = append(vs[:i], vs[i+1:]...)
		if problems := checkNormVectors(h, vs); !hasProblem(problems, "syntax-domain-256-ascii", "is missing") {
			t.Errorf("a deleted boundary vector was not reported by name: %v", problems)
		}
	})
	t.Run("the file and the rule are edited together (a flipped boundary expectation under a mutant rule)", func(t *testing.T) {
		vs := cloneVectors(f.Vectors)
		i := named("syntax-local-65-ascii")
		b := true // the mutant rule below accepts 65 bytes, and the file is edited to agree
		vs[i].SyntaxOK = &b
		mutant := func(local, domain string) bool {
			return len(local) >= 1 && len(local) <= 65 && len(domain) >= 1 && len(domain) <= 255 && strings.Contains(domain, ".")
		}
		problems := checkNormVectorsWith(h, vs, mutant)
		if !hasProblem(problems, "syntax-local-65-ascii", "must carry syntax_ok = false") {
			t.Errorf("a boundary vector edited together with the rule was not reported by the pinned table: %v", problems)
		}
	})
	t.Run("the table holds only true outcomes", func(t *testing.T) {
		vs := cloneVectors(f.Vectors)
		for i := range vs {
			if !vs[i].Malformed {
				b := true
				vs[i].SyntaxOK = &b
			}
		}
		if problems := checkNormVectorsWith(h, vs, func(string, string) bool { return true }); !hasProblem(problems, "syntax floor:") {
			t.Errorf("a one-sided syntax table was not reported: %v", problems)
		}
	})

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

// A ONE-CHARACTER MUTANT OF THE SYNTAX RULE is caught, by the vector that pins the changed boundary. Each mutant below is the shipped rule
// (layers/cheap_layers.go SyntaxLayer) with one thing changed; the real file must be silent under the shipped rule and report the named vector
// under each mutant. So a platform implementation (or a future edit of the layer) that drifts by one byte, or measures runes instead of bytes,
// or counts the dot of a trailing-dot domain, cannot pass this file.
func TestNormVectors_SyntaxRule_AMutantIsReportedByTheBoundaryVectorThatPinsIt(t *testing.T) {
	h := newTestHasher(t)
	f := loadNormVectors(t)
	if p := checkNormVectorsWith(h, f.Vectors, realSyntaxRule); len(p) != 0 {
		t.Fatalf("control setup: the shipped rule must be clean on the real file, got %v", p)
	}
	// The reference spelling of the shipped rule, used only to build the mutants from.
	rule := func(maxLocal, maxDomain int, countRunes bool, needDot bool) syntaxRule {
		return func(local, domain string) bool {
			ll := len(local)
			if countRunes {
				ll = len([]rune(local))
			}
			return local != "" && domain != "" && ll <= maxLocal && len(domain) <= maxDomain && (!needDot || strings.Contains(domain, "."))
		}
	}
	if !agreesOnFile(f.Vectors, h, rule(64, 255, false, true)) {
		t.Fatal("control setup: the reference spelling of the rule does not reproduce the shipped rule on the file")
	}
	mutants := []struct {
		name string
		rule syntaxRule
		want string // a vector the report must name
	}{
		{"local limit 64 -> 65", rule(65, 255, false, true), "syntax-local-65-ascii"},
		{"local limit 64 -> 63", rule(63, 255, false, true), "syntax-local-64-ascii"},
		{"domain limit 255 -> 256", rule(64, 256, false, true), "syntax-domain-256-ascii"},
		{"domain limit 255 -> 254", rule(64, 254, false, true), "syntax-domain-255-ascii"},
		{"local part counted in runes, not bytes", rule(64, 255, true, true), "syntax-local-66-bytes-33-runes"},
		{"the dot rule dropped", rule(64, 255, false, false), "syntax-domain-no-dot"},
		{"the dot rule dropped (trailing-dot case)", rule(64, 255, false, false), "syntax-domain-only-a-trailing-dot"},
	}
	for _, m := range mutants {
		t.Run(m.name, func(t *testing.T) {
			problems := checkNormVectorsWith(h, f.Vectors, m.rule)
			if !hasProblem(problems, m.want, "syntax_ok = ") {
				t.Errorf("the mutant %q was not reported by %s: %v", m.name, m.want, problems)
			}
		})
	}
	// A mutant that judges the RAW input instead of the normalised parts is caught too: the dot of 'b.' and the length of a decomposed local part
	// only exist before normalisation.
	t.Run("the rule judged on the raw input, not the normalised parts", func(t *testing.T) {
		raw := map[string]string{}
		for _, v := range f.Vectors {
			if v.Malformed {
				continue
			}
			in, _ := v.rawInput()
			n, _, _ := h.Normalize(in)
			raw[n] = in
		}
		// Several vectors share a normalised string, so key the mutant on the normalised address and judge the LAST raw input seen for it:
		// enough for the two vectors named below, whose normalised strings are unique to them.
		rawRule := func(local, domain string) bool {
			in, ok := raw[local+"@"+domain]
			if !ok {
				return realSyntaxRule(local, domain)
			}
			at := strings.LastIndex(in, "@")
			return at > 0 && at < len(in)-1 && len(in[:at]) <= 64 && len(in[at+1:]) <= 255 && strings.Contains(in[at+1:], ".")
		}
		problems := checkNormVectorsWith(h, f.Vectors, rawRule)
		if !hasProblem(problems, "syntax_ok = ") {
			t.Errorf("a rule judged on the raw input was not reported by any vector: %v", problems)
		}
	})
}

func agreesOnFile(vs []normVector, h *Hasher, r syntaxRule) bool {
	return len(checkNormVectorsWith(h, vs, r)) == 0
}

// SplitNormalized is the orchestrator's split. It splits at the LAST '@' (which is THE '@' of every address Normalize returns) and agrees with the
// domain Normalize returns on every accepted vector of the shared file; the control splits at the first '@' and must disagree on a string with two.
func TestSplitNormalized(t *testing.T) {
	cases := []struct{ in, local, domain string }{
		{"a@b.c", "a", "b.c"},
		{"jane.doe@example.com", "jane.doe", "example.com"},
		{"a+tag@example.com", "a+tag", "example.com"},
		{"x@y@z", "x@y", "z"}, // never returned by Normalize; pins "last '@'"
		{"@b", "", "b"},
		{"a@", "a", ""},
		{"no-at-sign", "", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		if l, d := SplitNormalized(c.in); l != c.local || d != c.domain {
			t.Errorf("SplitNormalized(%q) = (%q, %q), want (%q, %q)", c.in, l, d, c.local, c.domain)
		}
	}
	// CONTROL: a split at the FIRST '@' gives another answer on a string with two, so the case above can fail.
	first := func(s string) (string, string) {
		at := strings.Index(s, "@")
		if at < 0 {
			return "", ""
		}
		return s[:at], s[at+1:]
	}
	if l, d := first("x@y@z"); l == "x@y" && d == "z" {
		t.Error("control setup: a first-'@' split must differ from the last-'@' split on x@y@z")
	}
	// Equivalence with the inline expression it replaced (`normalized[:indexAt(normalized)]`, indexAt = last '@' or 0) on every normalised string
	// of the shared file.
	old := func(s string) string {
		at := 0
		for i := len(s) - 1; i >= 0; i-- {
			if s[i] == '@' {
				at = i
				break
			}
		}
		return s[:at]
	}
	f := loadNormVectors(t)
	n := 0
	for _, v := range f.Vectors {
		if v.Malformed || v.Normalized == nil {
			continue
		}
		n++
		if l, _ := SplitNormalized(*v.Normalized); l != old(*v.Normalized) {
			t.Errorf("%s: SplitNormalized local part %q differs from the old inline split %q", v.Name, l, old(*v.Normalized))
		}
	}
	if n < normVectorsFloor/2 {
		t.Fatalf("only %d normalised strings were compared with the old split", n)
	}
}
