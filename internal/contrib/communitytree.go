package contrib

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kodestar/audiosilo-meta/pkg/check"
	"github.com/kodestar/audiosilo-meta/pkg/pack"
)

// The community repository (KodeStar/audiosilo-meta-community) holds ONE pack
// family, works-community: data/works-community/<dir-bound>/<bound>.json, each
// {"entries": {"<work-slug>": {"characters": {...}, "recaps": {...},
// "description": {...}}}} (audiosilo-meta PACK-SPEC.md). A sidecar is a MEMBER of
// its work's entry, so a direct pull request is a read-modify-write of that entry
// in whichever pack holds its slug range - and a write may split a pack. Nothing
// here computes placement by hand: the edit goes through meta's own pkg/pack Store
// (placement, due splits, canonical rendering) and is validated by its own
// pkg/check before a byte is committed.

// communityDataPrefix is the repository-relative family root this edit reads.
const communityDataPrefix = "data/" + string(pack.FamilyWorksCommunity) + "/"

// Bounds on the community tarball: the whole data tree is tens of megabytes today
// (46 MB, 155 packs at the time of writing), so these are ceilings against a
// hostile or broken archive, not budgets.
const (
	maxCommunityTarball   = 512 << 20 // compressed bytes read from the network
	maxCommunityFileBytes = 32 << 20  // one pack file (the hard pack cap is 512 KB)
	maxCommunityTreeBytes = 1 << 30   // all extracted pack files together
	maxCommunityFiles     = 100_000
)

// SidecarMember is one sidecar to place in a work's works-community entry: Member is
// the entry member name ("characters" | "recaps"), Content the sidecar file's JSON
// (exactly what an intake issue attaches).
type SidecarMember struct {
	Member  string
	Content []byte
}

// CommunityEdit is a prepared pull-request change to the community repository.
type CommunityEdit struct {
	// Files are the pack files to write, repository-relative ("data/works-community/
	// ..."), with their full new contents.
	Files map[string][]byte
	// Deleted are the repository-relative pack files the edit removes (a split
	// renames or retires a pack).
	Deleted []string
	// Applied are the members placed, in the order given.
	Applied []string
	// Refused maps a member NOT placed to the reason: the entry already carries that
	// member. Replacing a sidecar is the maintainers' call, so it is never
	// overwritten here; the caller sends that dimension through the intake issue
	// path instead, where the bot routes it to a human.
	Refused map[string]string
	// Pack is the repository-relative pack file that holds the work's entry after
	// the edit ("" when nothing was applied).
	Pack string
}

// Changed reports whether the edit writes or removes anything.
func (e CommunityEdit) Changed() bool { return len(e.Files) > 0 || len(e.Deleted) > 0 }

// ErrCommunityTreeInvalid wraps a post-edit validation failure: the edited tree did
// not pass meta's own check, so no pull request may be opened from it.
var ErrCommunityTreeInvalid = errors.New("contrib: edited community tree does not validate")

// PrepareCommunityEdit downloads repo's community data tree at ref, places members in
// workSlug's works-community entry, and returns the resulting change. It never
// touches GitHub beyond the one read; the caller commits the change.
func PrepareCommunityEdit(ctx context.Context, cli *Client, repo, ref, workSlug string, members []SidecarMember) (CommunityEdit, error) {
	tmp, err := os.MkdirTemp("", "audiosilo-community-*")
	if err != nil {
		return CommunityEdit{}, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	dataDir := filepath.Join(tmp, "data")
	if err := cli.Tarball(ctx, repo, ref, maxCommunityTarball, func(r io.Reader) error {
		return ExtractCommunityData(r, dataDir)
	}); err != nil {
		return CommunityEdit{}, fmt.Errorf("contrib: fetch community tree: %w", err)
	}
	return EditCommunityTree(dataDir, workSlug, members)
}

// ExtractCommunityData unpacks the works-community pack files of a GitHub repository
// tarball (gzip; every entry under one "<owner>-<repo>-<sha>/" top directory) into
// dataDir, laid out as the data root (dataDir/works-community/...). Everything
// else in the archive is skipped. Only regular .json files are written, a path
// that is absolute or climbs out is refused, and the sizes are capped.
func ExtractCommunityData(r io.Reader, dataDir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("contrib: tarball: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	var total int64
	files := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("contrib: tarball: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		name := hdr.Name
		// Drop GitHub's single top-level directory.
		i := strings.IndexByte(name, '/')
		if i < 0 {
			continue
		}
		rel := name[i+1:]
		if !strings.HasPrefix(rel, communityDataPrefix) || !strings.HasSuffix(rel, ".json") {
			continue
		}
		clean := path.Clean(rel)
		if clean != rel || strings.HasPrefix(clean, "/") || slices.Contains(strings.Split(clean, "/"), "..") {
			return fmt.Errorf("contrib: tarball: refusing path %q", name)
		}
		if hdr.Size > maxCommunityFileBytes {
			return fmt.Errorf("contrib: tarball: %s is %d bytes, over the %d-byte cap", rel, hdr.Size, maxCommunityFileBytes)
		}
		files++
		total += hdr.Size
		if files > maxCommunityFiles || total > maxCommunityTreeBytes {
			return errors.New("contrib: tarball: community tree over its size cap")
		}
		dst := filepath.Join(dataDir, filepath.FromSlash(strings.TrimPrefix(clean, "data/")))
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			return err
		}
		if err := writeCapped(dst, tr, hdr.Size); err != nil {
			return err
		}
	}
	return nil
}

// writeCapped copies exactly size bytes of r to a new file at dst.
func writeCapped(dst string, r io.Reader, size int64) error {
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644) //nolint:gosec // dst is confined under the temp data root
	if err != nil {
		return err
	}
	if _, err := io.CopyN(f, r, size); err != nil {
		_ = f.Close()
		return fmt.Errorf("contrib: tarball: %s: %w", filepath.Base(dst), err)
	}
	return f.Close()
}

// EditCommunityTree places members in workSlug's entry of the works-community tree
// at dataDir (a data root holding that family alone - meta's `community` tree
// profile), flushes through pkg/pack, and validates the result with pkg/check.
//
// The write is a read-modify-write of the ONE entry: every member already there (the
// sibling sidecar, the description) is carried through byte for byte, and a member
// being placed that the entry ALREADY carries is refused rather than overwritten.
// Nothing is flushed when no member applies. A tree that does not validate after the
// edit is ErrCommunityTreeInvalid, naming the first problems.
func EditCommunityTree(dataDir, workSlug string, members []SidecarMember) (CommunityEdit, error) {
	edit := CommunityEdit{Files: map[string][]byte{}, Refused: map[string]string{}}
	st, err := pack.OpenProfile(dataDir, pack.ProfileCommunity)
	if err != nil {
		return edit, fmt.Errorf("contrib: open community tree: %w", err)
	}
	fam := pack.FamilyWorksCommunity
	entry := map[string]json.RawMessage{}
	raw, found, err := st.Get(fam, workSlug)
	if err != nil {
		return edit, fmt.Errorf("contrib: read %s entry %q: %w", fam, workSlug, err)
	}
	if found {
		if err := json.Unmarshal(raw, &entry); err != nil {
			return edit, fmt.Errorf("contrib: parse %s entry %q: %w", fam, workSlug, err)
		}
	}
	for _, m := range members {
		if _, taken := entry[m.Member]; taken {
			edit.Refused[m.Member] = fmt.Sprintf("the community tree already holds a %s sidecar for %s - replacing it is the maintainers' call", m.Member, workSlug)
			continue
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(m.Content, &obj); err != nil {
			return edit, fmt.Errorf("contrib: %s sidecar is not a JSON object: %w", m.Member, err)
		}
		var work string
		if err := json.Unmarshal(obj["work"], &work); err != nil || work != workSlug {
			return edit, fmt.Errorf("contrib: %s sidecar names work %q, want %q", m.Member, work, workSlug)
		}
		entry[m.Member] = json.RawMessage(m.Content)
		edit.Applied = append(edit.Applied, m.Member)
	}
	if len(edit.Applied) == 0 {
		return edit, nil
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return edit, err
	}
	if err := st.Upsert(fam, workSlug, encoded); err != nil {
		return edit, fmt.Errorf("contrib: queue %s entry %q: %w", fam, workSlug, err)
	}
	written, err := st.Flush()
	if err != nil {
		return edit, fmt.Errorf("contrib: flush community tree: %w", err)
	}
	if res := check.LoadProfile(dataDir, pack.ProfileCommunity); len(res.Problems) > 0 {
		lines := make([]string, 0, 3)
		for i, p := range res.Problems {
			if i == 3 {
				lines = append(lines, fmt.Sprintf("and %d more", len(res.Problems)-3))
				break
			}
			lines = append(lines, p.String())
		}
		return edit, fmt.Errorf("%w: %s", ErrCommunityTreeInvalid, strings.Join(lines, "; "))
	}
	for _, rel := range written.Wrote {
		content, err := os.ReadFile(filepath.Join(dataDir, filepath.FromSlash(rel))) //nolint:gosec // a path pkg/pack just wrote under the temp root
		if err != nil {
			return edit, err
		}
		edit.Files["data/"+rel] = content
	}
	for _, rel := range written.Deleted {
		edit.Deleted = append(edit.Deleted, "data/"+rel)
	}
	if ref, err := st.Locate(fam, workSlug); err == nil {
		edit.Pack = "data/" + ref.Path()
	}
	return edit, nil
}
