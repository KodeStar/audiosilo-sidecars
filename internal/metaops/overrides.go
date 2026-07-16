package metaops

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Override-upsert sentinel errors. The API handler maps these to HTTP status
// codes; ErrWorkNotFound and ErrDisabled (defined in coverage.go) complete the
// taxonomy - unknown work id (400) and disabled service (503) respectively.
var (
	// ErrNoSourcePath is a request with an empty source_path (400).
	ErrNoSourcePath = errors.New("source_path is required")
	// ErrPathNotAllowed is a source_path outside the configured library roots (403).
	ErrPathNotAllowed = errors.New("path is outside the configured library roots")
	// ErrUpstream wraps an unreachable/failed metadata lookup (502), kept distinct
	// from a persistence failure so the handler can tell 502 from 500.
	ErrUpstream = errors.New("metadata service is unreachable")
)

// StoredOverride is a persisted override row, the metaops mirror of the store's
// row so this package stays store-agnostic (the persist func translates).
type StoredOverride struct {
	SourcePath string
	Hidden     bool
	WorkID     string
	WorkTitle  string
	UpdatedAt  string
}

// OverrideRequest is the desired override state for a source path.
type OverrideRequest struct {
	SourcePath string
	Hidden     bool
	WorkID     string
}

// OverrideResult is a completed upsert: the persisted row plus the recomputed
// coverage (non-nil only for a manual work match).
type OverrideResult struct {
	Override StoredOverride
	Coverage *Coverage
}

// PersistFunc writes the desired override state and returns the stored row. It is
// injected so metaops never imports the store package.
type PersistFunc func(ctx context.Context, ov StoredOverride) (StoredOverride, error)

// OverrideService orchestrates a candidate-override upsert: it enforces the
// library-roots allow-list, resolves a manual work match against the metadata
// client, persists the desired state, and reflects it on the in-memory scan
// jobs. It keeps this workflow out of the transport layer (the API handler over
// it is pure decode -> call -> status-map -> encode).
type OverrideService struct {
	meta    *Client
	scans   *ScanManager
	persist PersistFunc
}

// NewOverrideService wires the service. meta may be nil (metadata unconfigured);
// a manual match then reports ErrDisabled.
func NewOverrideService(meta *Client, scans *ScanManager, persist PersistFunc) *OverrideService {
	return &OverrideService{meta: meta, scans: scans, persist: persist}
}

// Upsert applies the full desired override state for req.SourcePath, checked
// against roots. A manual match (non-empty WorkID) is validated against the
// metadata client and its coverage returned. Errors: ErrNoSourcePath /
// ErrPathNotAllowed (caller input), ErrWorkNotFound / ErrDisabled (a manual
// match against a stale id or a disabled service), ErrUpstream (unreachable
// upstream), or a raw error from the persist func (a storage failure).
func (s *OverrideService) Upsert(ctx context.Context, req OverrideRequest, roots []string) (OverrideResult, error) {
	sp := strings.TrimSpace(req.SourcePath)
	if sp == "" {
		return OverrideResult{}, ErrNoSourcePath
	}
	// Canonicalize the source path (abs + symlink-eval + clean) so the persisted
	// key matches the canonical SourcePath a scan computes (ScanManager.Start
	// canonicalizes its root the same way). Without this, a trailing-slash or
	// symlinked spelling stores an override the scan overlay never finds.
	if canon, err := resolvePath(sp); err == nil {
		sp = canon
	}
	if ok, err := PathAllowed(sp, roots); err != nil || !ok {
		return OverrideResult{}, ErrPathNotAllowed
	}

	workID := strings.TrimSpace(req.WorkID)
	var cov *Coverage
	var workTitle string
	if workID != "" {
		if s.meta == nil || !s.meta.Enabled() {
			return OverrideResult{}, ErrDisabled
		}
		c, err := s.meta.CoverageForWork(ctx, workID)
		switch {
		case errors.Is(err, ErrWorkNotFound):
			return OverrideResult{}, ErrWorkNotFound
		case errors.Is(err, ErrDisabled):
			return OverrideResult{}, ErrDisabled
		case err != nil:
			return OverrideResult{}, fmt.Errorf("%w: %v", ErrUpstream, err)
		}
		cov = &c
		workTitle = c.WorkTitle
	}

	stored, err := s.persist(ctx, StoredOverride{
		SourcePath: sp, Hidden: req.Hidden, WorkID: workID, WorkTitle: workTitle,
	})
	if err != nil {
		return OverrideResult{}, err
	}
	// Reflect the change on any in-memory scan jobs (cheap read-time overlay).
	s.scans.ApplyOverride(sp, req.Hidden, cov)
	return OverrideResult{Override: stored, Coverage: cov}, nil
}
