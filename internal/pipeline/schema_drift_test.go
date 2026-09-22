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

	// The share-alike license enum (referenced by both sidecar schemas via
	// common.schema.json#/$defs/license_content) is a single value. It must equal the
	// local constant, with ONE documented exception: the pinned module predates the
	// community layer's move to CC BY-SA 4.0 - see stalePinLicenseContent.
	lc := asObj(t, asObj(t, asObj(t, common)["$defs"])["license_content"])
	enum, _ := lc["enum"].([]any)
	if len(enum) != 1 {
		t.Fatalf("license_content enum = %v, want exactly one value", enum)
	}
	// The pinned value may lag: audiosilo-meta moved the community layer to CC BY-SA
	// 4.0 on 2026-08-21 (upstream commit 4a06b1a1, released in v0.13.0), but NO tag
	// carrying that change is consumable as a Go module - from v0.9.0 on, the repo's
	// data/ tree is ~1.6 GB, over the go command's 500 MiB module-zip ceiling
	// ("module source tree too large"), so `go get` fails for every 4.0-era tag. We
	// stay on v0.8.0 - whose characters/recaps schemas are byte-identical to
	// v0.15.0's - and emit the 4.0 value the intake requires. So the pinned enum is
	// accepted when it equals the local constant OR when it is that one known-stale
	// value; anything else is real upstream drift. Once upstream excludes data/ from
	// the module (a nested data/go.mod) and this pin moves forward, only the equality
	// can hold. TestSidecarLicenseIsTheCommunityLayerValue pins the emitted value.
	if v, _ := enum[0].(string); v != sidecarLicenseContent && v != stalePinLicenseContent {
		t.Errorf("license_content enum[0] = %q, local sidecarLicenseContent = %q", v, sidecarLicenseContent)
	}
}

// stalePinLicenseContent is the license value of the PINNED audiosilo-meta module
// (v0.8.0), which predates the community layer's move to CC BY-SA 4.0. It is not a
// value this tool may ever emit - see TestSidecarLicenseIsTheCommunityLayerValue.
const stalePinLicenseContent = "CC-BY-SA-3.0"

// TestSidecarLicenseIsTheCommunityLayerValue pins the emitted license string to the
// value KodeStar/audiosilo-meta-community's intake requires. The upstream schema
// cannot enforce it here (the module pin is stale, see above), so this is the guard
// that stops a silent revert to 3.0 - the exact failure the intake bot reports as
// "characters: /license: value must be 'CC-BY-SA-4.0'".
func TestSidecarLicenseIsTheCommunityLayerValue(t *testing.T) {
	if sidecarLicenseContent != "CC-BY-SA-4.0" {
		t.Errorf("sidecarLicenseContent = %q, want %q", sidecarLicenseContent, "CC-BY-SA-4.0")
	}
	if sidecarLicenseContent == stalePinLicenseContent {
		t.Error("sidecarLicenseContent is the retired 3.0 value the community intake rejects")
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
