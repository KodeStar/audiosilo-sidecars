package transcript

import "bytes"

// nonFinite are the JSON-invalid numeric literals whisper backends (MLX in
// particular) can emit for values like avg_logprob. Longer literals come first so
// "-Infinity" is matched before "Infinity".
var nonFinite = [][]byte{[]byte("-Infinity"), []byte("Infinity"), []byte("NaN")}

// Sanitize returns raw with every non-finite numeric literal (NaN, Infinity,
// -Infinity) that appears OUTSIDE a JSON string replaced by null, so the result
// parses under encoding/json (which rejects those tokens). It is string-aware: a
// literal that happens to appear inside transcript text (e.g. the word "NaN") is
// left untouched, and JSON escaping within strings is honored. Bytes that are
// already valid JSON pass through unchanged. This mirrors the historical
// sanitize step (parse_constant -> None) while preserving the raw evidence, which
// the caller keeps byte-for-byte in transcripts-raw/.
func Sanitize(raw []byte) []byte {
	// Fast path: nothing to do when no literal is present at all.
	if !containsAny(raw, nonFinite) {
		return raw
	}
	out := make([]byte, 0, len(raw))
	inString := false
	escaped := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if inString {
			out = append(out, c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			out = append(out, c)
			continue
		}
		if n := matchNonFinite(raw, i); n > 0 {
			out = append(out, 'n', 'u', 'l', 'l')
			i += n - 1
			continue
		}
		out = append(out, c)
	}
	return out
}

// matchNonFinite returns the length of the non-finite literal starting at raw[i],
// or 0 if none. It only matches when the position is a literal boundary (the token
// stands alone, not as part of a longer identifier), which outside a JSON string
// it always is where a number would appear.
func matchNonFinite(raw []byte, i int) int {
	for _, lit := range nonFinite {
		if bytes.HasPrefix(raw[i:], lit) {
			return len(lit)
		}
	}
	return 0
}

func containsAny(raw []byte, lits [][]byte) bool {
	for _, lit := range lits {
		if bytes.Contains(raw, lit) {
			return true
		}
	}
	return false
}
