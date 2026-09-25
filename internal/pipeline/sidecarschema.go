package pipeline

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"

	meta "github.com/kodestar/audiosilo-meta"
)

// sidecarSchemas compiles meta's embedded characters/recaps schemas (plus the
// common.schema.json they $ref) once per process, keyed by sidecar kind. Each is
// registered under its own `$id`, which is what their `$ref`s resolve against.
var sidecarSchemas = sync.OnceValues(func() (map[string]*jsonschema.Schema, error) {
	c := jsonschema.NewCompiler()
	// Assert "format" as metacheck does, so this gate and the intake agree.
	c.AssertFormat()
	ids := map[string]string{}
	for _, f := range []string{"common", "characters", "recaps"} {
		name := f + ".schema.json"
		raw, err := meta.SchemaFS.ReadFile("schema/" + name)
		if err != nil {
			return nil, fmt.Errorf("read embedded schema %s: %w", name, err)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("parse schema %s: %w", name, err)
		}
		obj, _ := doc.(map[string]any)
		id, _ := obj["$id"].(string)
		if id == "" {
			return nil, fmt.Errorf("schema %s declares no $id", name)
		}
		if err := c.AddResource(id, doc); err != nil {
			return nil, fmt.Errorf("add schema %s: %w", name, err)
		}
		ids[f] = id
	}
	out := map[string]*jsonschema.Schema{}
	for _, k := range []string{"characters", "recaps"} {
		sch, err := c.Compile(ids[k])
		if err != nil {
			return nil, fmt.Errorf("compile %s schema: %w", k, err)
		}
		out[k] = sch
	}
	return out, nil
})

// schemaLeaf is one most-specific schema violation, rendered "location: message".
// refused marks the kinds extract.NGram hard-fails on (see schemaViolation).
type schemaLeaf struct {
	text    string
	refused bool
}

// schemaViolation validates raw against sch. It returns a one-line description of
// the first violation plus a count of the rest ("" when raw is valid), and whether
// extract.NGram would REFUSE the file: not valid JSON, a missing top-level required
// key (NGram's kind discriminator), or a wrong JSON type anywhere, null included
// (NGram's per-field type checks - a superset, which errs toward skipping). Any other
// violation (a cap, an enum, a minLength) NGram scans through; the structural checks
// report those. When refused, the violation named is the first REFUSED one.
//
// It walks the DETAILED output: the basic output reduces every failure behind a $ref
// to "validation failed". "First" is the smallest leaf in (instance location,
// message) order, not the validator's: it walks an object's properties in Go map
// order, so its own first leaf differs run to run over the same file.
func schemaViolation(sch *jsonschema.Schema, raw []byte) (violation string, refused bool) {
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return "not valid JSON: " + oneLine(err.Error()), true
	}
	err = sch.Validate(inst)
	if err == nil {
		return "", false
	}
	var verr *jsonschema.ValidationError
	if !errors.As(err, &verr) {
		return oneLine(err.Error()), true
	}
	var leaves []schemaLeaf
	collectLeaves(verr.DetailedOutput(), &leaves)
	if len(leaves) == 0 {
		return oneLine(verr.Error()), true
	}
	slices.SortFunc(leaves, func(a, b schemaLeaf) int { return strings.Compare(a.text, b.text) })
	leaves = slices.CompactFunc(leaves, func(a, b schemaLeaf) bool { return a.text == b.text })
	first := leaves[0]
	for _, l := range leaves {
		if l.refused {
			first = l
			break
		}
	}
	if len(leaves) == 1 {
		return first.text, first.refused
	}
	return fmt.Sprintf("%s (and %d more)", first.text, len(leaves)-1), first.refused
}

// collectLeaves appends every output unit carrying an error of its own and no
// nested errors - the most specific reasons in the tree.
func collectLeaves(u *jsonschema.OutputUnit, out *[]schemaLeaf) {
	if u == nil {
		return
	}
	if u.Error != nil && len(u.Errors) == 0 {
		var refused bool
		switch u.Error.Kind.(type) {
		case *kind.Type:
			refused = true
		case *kind.Required:
			refused = u.InstanceLocation == ""
		}
		loc := u.InstanceLocation
		if loc == "" {
			loc = "/"
		}
		*out = append(*out, schemaLeaf{text: loc + ": " + oneLine(u.Error.String()), refused: refused})
		return
	}
	for i := range u.Errors {
		collectLeaves(&u.Errors[i], out)
	}
}

// oneLine flattens whitespace so a message fits one report line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
