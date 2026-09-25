package pipeline

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"

	meta "github.com/kodestar/audiosilo-meta"
)

// sidecarSchemaBase is the $id prefix meta's schemas reference each other by.
const sidecarSchemaBase = "https://meta.audiosilo.app/schema/"

// sidecarSchemas compiles meta's embedded characters/recaps schemas (plus the
// common.schema.json they $ref) once per process, keyed by sidecar kind.
var sidecarSchemas = sync.OnceValues(func() (map[string]*jsonschema.Schema, error) {
	c := jsonschema.NewCompiler()
	// Assert "format" as metacheck does, so this gate and the intake agree.
	c.AssertFormat()
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
		if err := c.AddResource(sidecarSchemaBase+name, doc); err != nil {
			return nil, fmt.Errorf("add schema %s: %w", name, err)
		}
	}
	out := map[string]*jsonschema.Schema{}
	for _, kind := range []string{"characters", "recaps"} {
		sch, err := c.Compile(sidecarSchemaBase + kind + ".schema.json")
		if err != nil {
			return nil, fmt.Errorf("compile %s schema: %w", kind, err)
		}
		out[kind] = sch
	}
	return out, nil
})

// firstSchemaViolation validates raw against sch and returns a one-line description
// of the first violation ("" when raw is valid). It walks the DETAILED output: the
// basic output reduces every failure behind a $ref to "validation failed".
func firstSchemaViolation(sch *jsonschema.Schema, raw []byte) string {
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return "not valid JSON: " + oneLine(err.Error())
	}
	err = sch.Validate(inst)
	if err == nil {
		return ""
	}
	var verr *jsonschema.ValidationError
	if !errors.As(err, &verr) {
		return oneLine(err.Error())
	}
	if leaf := firstLeaf(verr.DetailedOutput()); leaf != nil {
		loc := leaf.InstanceLocation
		if loc == "" {
			loc = "/"
		}
		return loc + ": " + oneLine(leaf.Error.String())
	}
	return oneLine(verr.Error())
}

// firstLeaf returns the first output unit carrying an error of its own and no
// nested errors - the most specific reason in the tree.
func firstLeaf(u *jsonschema.OutputUnit) *jsonschema.OutputUnit {
	if u == nil {
		return nil
	}
	if u.Error != nil && len(u.Errors) == 0 {
		return u
	}
	for i := range u.Errors {
		if leaf := firstLeaf(&u.Errors[i]); leaf != nil {
			return leaf
		}
	}
	return nil
}

// oneLine flattens whitespace so a message fits one report line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
