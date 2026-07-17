package pipeline

import (
	"encoding/json"
	"reflect"
	"testing"

	meta "github.com/kodestar/audiosilo-meta"
)

// TestSidecarConstantsMatchUpstreamSchema is a drift guard: the caps, enums, QID
// pattern and share-alike license in sidecars.go are HAND-COPIED from the
// audiosilo-meta characters/recaps schemas (there is no codegen). This test loads the
// authoritative schemas straight from the pinned meta module's embedded FS
// (meta.SchemaFS) and asserts the local constants still equal the upstream contract, so
// a silent upstream cap/enum change fails our build after a dep bump instead of shipping
// a sidecar the intake would reject.
func TestSidecarConstantsMatchUpstreamSchema(t *testing.T) {
	characters := loadSchema(t, "schema/characters.schema.json")
	recaps := loadSchema(t, "schema/recaps.schema.json")
	common := loadSchema(t, "schema/common.schema.json")

	charProps := itemProps(t, characters, "characters")
	recapProps := itemProps(t, recaps, "recaps")

	// Length caps (JSON-schema maxLength <-> the runeLen caps).
	assertMaxLength(t, "characters.description", charProps["description"], capDescription)
	assertMaxLength(t, "recaps.text", recapProps["text"], capRecapText)
	assertMaxLength(t, "recaps.in_short", topProps(t, recaps)["in_short"], capInShort)
	assertMaxLength(t, "recaps.ending", topProps(t, recaps)["ending"], capEnding)

	// Enums (schema enum <-> the local validRoles/validScopes sets).
	assertEnumSet(t, "characters.role", charProps["role"], validRoles)
	assertEnumSet(t, "recaps.scope", recapProps["scope"], validScopes)

	// The wikidata QID pattern (schema pattern <-> wikidataRe source).
	if got := asObj(t, charProps["xref"])["properties"]; got != nil {
		wd := asObj(t, asObj(t, got)["wikidata"])
		if pat, _ := wd["pattern"].(string); pat != wikidataRe.String() {
			t.Errorf("characters.xref.wikidata pattern = %q, local wikidataRe = %q", pat, wikidataRe.String())
		}
	} else {
		t.Fatal("characters.xref.properties missing in schema")
	}

	// The share-alike license enum must be exactly [CC-BY-SA-3.0] (referenced by both
	// sidecar schemas via common.schema.json#/$defs/license_content).
	lc := asObj(t, asObj(t, asObj(t, common)["$defs"])["license_content"])
	enum, _ := lc["enum"].([]any)
	if len(enum) != 1 {
		t.Fatalf("license_content enum = %v, want exactly one value", enum)
	}
	if v, _ := enum[0].(string); v != sidecarLicenseContent {
		t.Errorf("license_content enum[0] = %q, local sidecarLicenseContent = %q", v, sidecarLicenseContent)
	}
}

func loadSchema(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := meta.SchemaFS.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s from meta.SchemaFS: %v", name, err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return m
}

// topProps returns a schema's top-level "properties" object.
func topProps(t *testing.T, schema map[string]any) map[string]any {
	t.Helper()
	return asObj(t, schema["properties"])
}

// itemProps returns the per-item "properties" object of an array property (e.g. the
// shape of one character / one recap).
func itemProps(t *testing.T, schema map[string]any, arrayProp string) map[string]any {
	t.Helper()
	prop := asObj(t, topProps(t, schema)[arrayProp])
	items := asObj(t, prop["items"])
	return asObj(t, items["properties"])
}

func assertMaxLength(t *testing.T, locus string, prop any, want int) {
	t.Helper()
	// JSON numbers decode to float64.
	got, ok := asObj(t, prop)["maxLength"].(float64)
	if !ok {
		t.Fatalf("%s: maxLength missing in schema", locus)
	}
	if int(got) != want {
		t.Errorf("%s maxLength = %d, local cap = %d", locus, int(got), want)
	}
}

func assertEnumSet(t *testing.T, locus string, prop any, local map[string]bool) {
	t.Helper()
	rawEnum, ok := asObj(t, prop)["enum"].([]any)
	if !ok {
		t.Fatalf("%s: enum missing in schema", locus)
	}
	schemaSet := map[string]bool{}
	for _, v := range rawEnum {
		s, _ := v.(string)
		schemaSet[s] = true
	}
	if !reflect.DeepEqual(schemaSet, local) {
		t.Errorf("%s enum = %v, local set = %v", locus, schemaSet, local)
	}
}

func asObj(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("expected a JSON object, got %T", v)
	}
	return m
}
