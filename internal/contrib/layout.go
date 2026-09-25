package contrib

// LegacyShard is a verbatim copy of audiosilo-meta v0.8.0's model.Shard, for the
// retired per-record layout the contribution paths still address; temporary -
// see CLAUDE.md.
func LegacyShard(slug string) string {
	if len(slug) < 2 {
		return slug
	}
	return slug[:2]
}
