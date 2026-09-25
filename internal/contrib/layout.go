package contrib

// LegacyShard is the shard directory of audiosilo-meta's RETIRED file-per-record
// layout (data/works/<shard>/<slug>/work.json, characters.json, recaps.json): the
// slug's first two bytes, or the whole slug when it is shorter. It is a verbatim
// copy of the model.Shard that audiosilo-meta v0.8.0 exported.
//
// Upstream removed model.Shard (and the layout it addressed) when the data tree
// moved to range-packed storage (audiosilo-meta PACK-SPEC.md): a pack file holds
// many records, so a path no longer names one. The contribution paths that still
// spell per-record paths (the PR-mode file writes, the local export layout, the
// poller's work-slug-from-file-list resolution) keep their pre-pack behaviour
// through this copy; moving them onto the pack layout and the community
// repository is its own change, not part of the module bump that removed the
// upstream symbol.
func LegacyShard(slug string) string {
	if len(slug) < 2 {
		return slug
	}
	return slug[:2]
}
