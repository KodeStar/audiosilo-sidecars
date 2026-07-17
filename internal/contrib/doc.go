// Package contrib turns a finished book's validated CC BY-SA sidecars into a
// contribution to the community metadata database (KodeStar/audiosilo-meta): it
// composes prefilled intake-issue / add-work bodies that the meta repo's
// metaissue bot parses verbatim, and it drives the GitHub REST API (issues,
// gists, forks, refs, contents, pulls) with the stdlib HTTP client against an
// injectable base URL so every path is httptest-covered.
//
// Security invariant: the GitHub credential (a PAT from the secrets store or a
// token read from `gh auth token`) is carried ONLY in the Authorization request
// header. It never appears in argv, logs, or error strings - the gh shell-out
// reads the token from stdout and drops it on any failure path, and REST errors
// wrap the HTTP status plus a trimmed response body only (a GitHub response
// never echoes the bearer token). This mirrors internal/agent's rule that an
// injected secret never reaches argv/errors.
//
// Wave 1A added the token source, the REST client, and the body composers. Wave
// 3A adds the Service + intake poller (service.go / poller.go): the shared
// core-submit and status-polling logic. It imports the stdlib, the secrets store,
// the meta module's pkg/model (slug/shard rules), and internal/{store,state}; the
// scheduler and event hub are reached through injected Readmit/Publish function
// seams, so contrib imports neither scheduler nor pipeline (no import cycle: the
// pipeline contributing stage imports contrib, never the reverse).
package contrib
