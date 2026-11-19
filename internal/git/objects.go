package git

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"sort"
	"strconv"
	"strings"
	"sync"

	"gitforge/internal/store"
)

var ZeroOID [20]byte

var sha1Pool = sync.Pool{
	New: func() any {
		return sha1.New()
	},
}

func unhex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	default:
		return 0, false
	}
}

// ParseOIDBytes parses 40 hex ASCII bytes into a 20-byte array with zero allocations.
func ParseOIDBytes(b []byte) ([20]byte, bool) {
	var out [20]byte
	if len(b) != 40 {
		return out, false
	}
	for i := 0; i < 20; i++ {
		hi, ok1 := unhex(b[i*2])
		lo, ok2 := unhex(b[i*2+1])
		if !ok1 || !ok2 {
			return out, false
		}
		out[i] = (hi << 4) | lo
	}
	return out, true
}

// ParseOID parses a 40-char hex SHA-1 into a 20-byte array with zero allocations.
func ParseOID(s string) ([20]byte, bool) {
	var out [20]byte
	if len(s) != 40 {
		return out, false
	}
	for i := 0; i < 20; i++ {
		hi, ok1 := unhex(s[i*2])
		lo, ok2 := unhex(s[i*2+1])
		if !ok1 || !ok2 {
			return out, false
		}
		out[i] = (hi << 4) | lo
	}
	return out, true
}

// FormatOID formats a 20-byte SHA-1 as lowercase hex.
func FormatOID(oid [20]byte) string {
	return hex.EncodeToString(oid[:])
}

// HashObject computes Git's canonical SHA-1 object ID.
func HashObject(objType store.ObjType, data []byte) [20]byte {
	h := sha1Pool.Get().(hash.Hash)
	h.Reset()
	var hdr [48]byte
	b := hdr[:0]
	b = append(b, objType.String()...)
	b = append(b, ' ')
	b = strconv.AppendInt(b, int64(len(data)), 10)
	b = append(b, 0)
	_, _ = h.Write(b)
	_, _ = h.Write(data)
	var out [20]byte
	h.Sum(out[:0])
	sha1Pool.Put(h)
	return out
}

// TreeEntry represents one entry inside a Git tree object.
type TreeEntry struct {
	Mode string
	Name string
	OID  [20]byte
}

// ParseTree parses a raw Git tree payload.
func ParseTree(data []byte) ([]TreeEntry, error) {
	var entries []TreeEntry
	off := 0
	for off < len(data) {
		sp := bytes.IndexByte(data[off:], ' ')
		if sp < 0 {
			return nil, errors.New("tree entry missing space")
		}
		mode := string(data[off : off+sp])
		nameStart := off + sp + 1
		nul := bytes.IndexByte(data[nameStart:], 0)
		if nul < 0 {
			return nil, errors.New("tree entry missing nul")
		}
		name := string(data[nameStart : nameStart+nul])
		oidStart := nameStart + nul + 1
		if oidStart+20 > len(data) {
			return nil, errors.New("tree entry truncated oid")
		}
		var oid [20]byte
		copy(oid[:], data[oidStart:oidStart+20])
		entries = append(entries, TreeEntry{Mode: mode, Name: name, OID: oid})
		off = oidStart + 20
	}
	return entries, nil
}

func treeSortKey(e TreeEntry) string {
	if e.Mode == "40000" {
		return e.Name + "/"
	}
	return e.Name
}

// BuildTree serializes tree entries using Git's exact sorting order.
func BuildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		return treeSortKey(sorted[i]) < treeSortKey(sorted[j])
	})
	est := len(sorted) * 36
	out := make([]byte, 0, est)
	for _, e := range sorted {
		out = append(out, e.Mode...)
		out = append(out, ' ')
		out = append(out, e.Name...)
		out = append(out, 0)
		out = append(out, e.OID[:]...)
	}
	return out
}

// ParseCommit parses a raw Git commit payload eagerly into CommitInfo.
func ParseCommit(data []byte) (*store.CommitInfo, error) {
	blank := bytes.Index(data, []byte("\n\n"))
	hdr := data
	msg := ""
	if blank >= 0 {
		hdr = data[:blank]
		msg = string(data[blank+2:])
	}
	info := &store.CommitInfo{
		Message:    msg,
		MetaParsed: true,
	}
	parentCount := 0
	for len(hdr) > 0 {
		nl := bytes.IndexByte(hdr, '\n')
		var line []byte
		if nl < 0 {
			line = hdr
			hdr = nil
		} else {
			line = hdr[:nl]
			hdr = hdr[nl+1:]
		}
		if len(line) == 0 {
			continue
		}
		sp := bytes.IndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		key := line[:sp]
		val := line[sp+1:]
		switch string(key) {
		case "tree":
			oid, ok := ParseOIDBytes(val)
			if !ok {
				return nil, errors.New("invalid commit tree oid")
			}
			info.Tree = oid
		case "parent":
			oid, ok := ParseOIDBytes(val)
			if !ok {
				return nil, errors.New("invalid commit parent oid")
			}
			if parentCount < 2 && len(info.Parents) == parentCount {
				info.InlineParents[parentCount] = oid
				parentCount++
				info.Parents = info.InlineParents[:parentCount]
			} else {
				info.Parents = append(info.Parents, oid)
			}
		case "author":
			s := string(val)
			info.Author = s
			info.Timestamp = parseTimestamp(s)
		case "committer":
			s := string(val)
			info.Committer = s
			if info.Timestamp == 0 {
				info.Timestamp = parseTimestamp(s)
			}
		}
	}
	if info.Tree == ZeroOID {
		return nil, errors.New("commit missing tree")
	}
	return info, nil
}

// ParseCommitFastInto extracts only tree and parent OIDs from a commit with zero heap allocations.
// Commit message/author metadata stays lazy and can be decoded via CommitMessageLocked when queried.
func ParseCommitFastInto(data []byte, info *store.CommitInfo) error {
	hdr := data
	if blank := bytes.Index(data, []byte("\n\n")); blank >= 0 {
		hdr = data[:blank]
	}
	parentCount := 0
	info.Tree = ZeroOID
	info.Parents = nil
	info.MetaParsed = false

	for len(hdr) > 0 {
		nl := bytes.IndexByte(hdr, '\n')
		var line []byte
		if nl < 0 {
			line = hdr
			hdr = nil
		} else {
			line = hdr[:nl]
			hdr = hdr[nl+1:]
		}
		if len(line) >= 45 && bytes.HasPrefix(line, []byte("tree ")) {
			oid, ok := ParseOIDBytes(line[5:])
			if !ok {
				return errors.New("invalid commit tree oid")
			}
			info.Tree = oid
		} else if len(line) >= 47 && bytes.HasPrefix(line, []byte("parent ")) {
			oid, ok := ParseOIDBytes(line[7:])
			if !ok {
				return errors.New("invalid commit parent oid")
			}
			if parentCount < 2 && len(info.Parents) == parentCount {
				info.InlineParents[parentCount] = oid
				parentCount++
				info.Parents = info.InlineParents[:parentCount]
			} else {
				info.Parents = append(info.Parents, oid)
			}
		}
	}
	if info.Tree == ZeroOID {
		return errors.New("commit missing tree")
	}
	return nil
}

// CommitMessageLocked returns a commit's message (trailing newline stripped) without mutating ci.
func CommitMessageLocked(r *store.Repo, oid [20]byte, ci *store.CommitInfo) string {
	if ci != nil && ci.MetaParsed {
		return strings.TrimSuffix(ci.Message, "\n")
	}
	if obj := r.Objects[oid]; obj != nil {
		if blank := bytes.Index(obj.Data, []byte("\n\n")); blank >= 0 {
			msg := obj.Data[blank+2:]
			msg = bytes.TrimSuffix(msg, []byte{'\n'})
			return string(msg)
		}
	}
	return ""
}

func parseTimestamp(identityLine string) int64 {
	// Format: Name <email> 1735689600 +0000
	gt := strings.LastIndexByte(identityLine, '>')
	if gt < 0 {
		return 0
	}
	rest := strings.TrimSpace(identityLine[gt+1:])
	sp := strings.IndexByte(rest, ' ')
	if sp >= 0 {
		rest = rest[:sp]
	}
	ts, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		return 0
	}
	return ts
}

// BuildCommit creates a canonical Git commit payload.
func BuildCommit(tree [20]byte, parents [][20]byte, authorName, authorEmail string, timestamp int64, message string) []byte {
	if timestamp == 0 {
		timestamp = 1735689600
	}
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	ident := fmt.Sprintf("%s <%s> %d +0000", authorName, authorEmail, timestamp)
	var b strings.Builder
	b.WriteString("tree ")
	b.WriteString(FormatOID(tree))
	b.WriteByte('\n')
	for _, p := range parents {
		b.WriteString("parent ")
		b.WriteString(FormatOID(p))
		b.WriteByte('\n')
	}
	b.WriteString("author ")
	b.WriteString(ident)
	b.WriteByte('\n')
	b.WriteString("committer ")
	b.WriteString(ident)
	b.WriteString("\n\n")
	b.WriteString(message)
	return []byte(b.String())
}

// ParseTag parses a raw Git annotated tag object.
func ParseTag(data []byte) (target [20]byte, targetType store.ObjType, tagName string, err error) {
	blank := bytes.Index(data, []byte("\n\n"))
	hdrEnd := len(data)
	if blank >= 0 {
		hdrEnd = blank
	}
	for _, line := range strings.Split(string(data[:hdrEnd]), "\n") {
		if line == "" {
			continue
		}
		sp := strings.IndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		k := line[:sp]
		v := line[sp+1:]
		switch k {
		case "object":
			oid, ok := ParseOID(v)
			if !ok {
				return ZeroOID, 0, "", errors.New("invalid tag object oid")
			}
			target = oid
		case "type":
			switch v {
			case "commit":
				targetType = store.ObjCommit
			case "tree":
				targetType = store.ObjTree
			case "blob":
				targetType = store.ObjBlob
			case "tag":
				targetType = store.ObjTag
			}
		case "tag":
			tagName = v
		}
	}
	if target == ZeroOID {
		return ZeroOID, 0, "", errors.New("tag missing target object")
	}
	return target, targetType, tagName, nil
}

// PeelTagLocked follows tag objects until a non-tag target (usually a commit) is reached.
func PeelTagLocked(r *store.Repo, oid [20]byte) ([20]byte, bool) {
	cur := oid
	for i := 0; i < 16; i++ {
		obj, ok := r.Objects[cur]
		if !ok || obj.Type != store.ObjTag {
			break
		}
		next, _, _, err := ParseTag(obj.Data)
		if err != nil {
			return ZeroOID, false
		}
		cur = next
	}
	return cur, cur != oid
}

// PutObjectLocked hashes, stores, and indexes an object while r.GitMu is held.
func PutObjectLocked(r *store.Repo, objType store.ObjType, data []byte) ([20]byte, error) {
	oid := HashObject(objType, data)
	if _, exists := r.Objects[oid]; !exists {
		cp := make([]byte, len(data))
		copy(cp, data)
		r.Objects[oid] = &store.GitObject{Type: objType, Data: cp}
	}
	if objType == store.ObjCommit {
		if _, exists := r.Commits[oid]; !exists {
			info, err := ParseCommit(data)
			if err != nil {
				return ZeroOID, err
			}
			info.Height = computeCommitHeightLocked(r, info)
			r.Commits[oid] = info
		}
	}
	return oid, nil
}

func computeCommitHeightLocked(r *store.Repo, info *store.CommitInfo) uint32 {
	var maxP uint32
	for _, p := range info.Parents {
		if pi, ok := r.Commits[p]; ok && pi.Height > maxP {
			maxP = pi.Height
		}
	}
	return maxP + 1
}

// IsAncestorLocked checks if ancestor is an ancestor of descendant.
func IsAncestorLocked(r *store.Repo, ancestor, descendant [20]byte) bool {
	if ancestor == descendant {
		return true
	}
	descInfo := r.Commits[descendant]
	if descInfo == nil {
		return false
	}
	// Fast O(1) path for direct parent pushes (covers linear push benchmark).
	for _, p := range descInfo.Parents {
		if p == ancestor {
			return true
		}
	}
	ancInfo := r.Commits[ancestor]
	ancHeight := uint32(0)
	if ancInfo != nil {
		ancHeight = ancInfo.Height
	}

	visited := make(map[[20]byte]struct{}, 32)
	stack := make([][20]byte, 0, 32)
	stack = append(stack, descendant)
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == ancestor {
			return true
		}
		if _, seen := visited[cur]; seen {
			continue
		}
		visited[cur] = struct{}{}
		ci := r.Commits[cur]
		if ci == nil {
			continue
		}
		if ancHeight > 0 && ci.Height <= ancHeight {
			continue
		}
		for _, p := range ci.Parents {
			if p == ancestor {
				return true
			}
			if _, seen := visited[p]; !seen {
				stack = append(stack, p)
			}
		}
	}
	return false
}

// MergeBaseLocked computes a lowest common ancestor of commits a and b.
func MergeBaseLocked(r *store.Repo, a, b [20]byte) ([20]byte, bool) {
	if a == b {
		return a, true
	}
	ancA := make(map[[20]byte]int)
	qA := [][20]byte{a}
	ancA[a] = 0
	for len(qA) > 0 {
		cur := qA[0]
		qA = qA[1:]
		d := ancA[cur]
		ci := r.Commits[cur]
		if ci == nil {
			continue
		}
		for _, p := range ci.Parents {
			if _, seen := ancA[p]; !seen {
				ancA[p] = d + 1
				qA = append(qA, p)
			}
		}
	}

	seenB := make(map[[20]byte]struct{})
	qB := [][20]byte{b}
	seenB[b] = struct{}{}
	for len(qB) > 0 {
		cur := qB[0]
		qB = qB[1:]
		if _, ok := ancA[cur]; ok {
			return cur, true
		}
		ci := r.Commits[cur]
		if ci == nil {
			continue
		}
		for _, p := range ci.Parents {
			if _, seen := seenB[p]; !seen {
				seenB[p] = struct{}{}
				qB = append(qB, p)
			}
		}
	}
	return ZeroOID, false
}

// FlatEntry is a single leaf file inside a flattened commit tree.
type FlatEntry struct {
	Mode string
	OID  [20]byte
}

// FlattenCommitTreeLocked walks a commit's tree recursively into path -> FlatEntry.
func FlattenCommitTreeLocked(r *store.Repo, commitOID [20]byte) (map[string]FlatEntry, error) {
	out := make(map[string]FlatEntry)
	if commitOID == ZeroOID {
		return out, nil
	}
	ci := r.Commits[commitOID]
	if ci == nil {
		return nil, fmt.Errorf("missing commit %s", FormatOID(commitOID))
	}
	var walk func(prefix string, treeOID [20]byte) error
	walk = func(prefix string, treeOID [20]byte) error {
		obj := r.Objects[treeOID]
		if obj == nil || obj.Type != store.ObjTree {
			return fmt.Errorf("missing tree %s", FormatOID(treeOID))
		}
		entries, err := ParseTree(obj.Data)
		if err != nil {
			return err
		}
		for _, e := range entries {
			full := e.Name
			if prefix != "" {
				full = prefix + "/" + e.Name
			}
			if e.Mode == "40000" {
				if err := walk(full, e.OID); err != nil {
					return err
				}
			} else {
				out[full] = FlatEntry{Mode: e.Mode, OID: e.OID}
			}
		}
		return nil
	}
	if err := walk("", ci.Tree); err != nil {
		return nil, err
	}
	return out, nil
}

// WriteTreeFromFlatLocked constructs nested Git tree objects from a flat path map.
func WriteTreeFromFlatLocked(r *store.Repo, files map[string]FlatEntry) ([20]byte, error) {
	type dirNode struct {
		files map[string]FlatEntry
		dirs  map[string]*dirNode
	}
	newNode := func() *dirNode {
		return &dirNode{
			files: make(map[string]FlatEntry),
			dirs:  make(map[string]*dirNode),
		}
	}
	root := newNode()
	for p, entry := range files {
		clean := strings.Trim(p, "/")
		if clean == "" {
			continue
		}
		segs := strings.Split(clean, "/")
		cur := root
		for i := 0; i < len(segs)-1; i++ {
			seg := segs[i]
			next := cur.dirs[seg]
			if next == nil {
				next = newNode()
				cur.dirs[seg] = next
			}
			cur = next
		}
		cur.files[segs[len(segs)-1]] = entry
	}
	var build func(n *dirNode) ([20]byte, error)
	build = func(n *dirNode) ([20]byte, error) {
		entries := make([]TreeEntry, 0, len(n.files)+len(n.dirs))
		for name, f := range n.files {
			mode := f.Mode
			if mode == "" {
				mode = "100644"
			}
			entries = append(entries, TreeEntry{Mode: mode, Name: name, OID: f.OID})
		}
		for name, d := range n.dirs {
			subOID, err := build(d)
			if err != nil {
				return ZeroOID, err
			}
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: subOID})
		}
		raw := BuildTree(entries)
		return PutObjectLocked(r, store.ObjTree, raw)
	}
	return build(root)
}

// FileDiff describes one changed file between two commits.
type FileDiff struct {
	Filename  string
	Status    string
	Additions int
	Deletions int
	Changes   int
	Patch     string
}

func countLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	n := bytes.Count(data, []byte{'\n'})
	if data[len(data)-1] != '\n' {
		n++
	}
	return n
}

// DiffCommitsLocked computes file-level diffs from baseCommit to headCommit.
func DiffCommitsLocked(r *store.Repo, baseCommit, headCommit [20]byte) ([]FileDiff, error) {
	baseFiles, err := FlattenCommitTreeLocked(r, baseCommit)
	if err != nil {
		return nil, err
	}
	headFiles, err := FlattenCommitTreeLocked(r, headCommit)
	if err != nil {
		return nil, err
	}
	allPaths := make(map[string]struct{}, len(baseFiles)+len(headFiles))
	for p := range baseFiles {
		allPaths[p] = struct{}{}
	}
	for p := range headFiles {
		allPaths[p] = struct{}{}
	}
	paths := make([]string, 0, len(allPaths))
	for p := range allPaths {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var diffs []FileDiff
	for _, p := range paths {
		bf, inBase := baseFiles[p]
		hf, inHead := headFiles[p]
		if inBase && inHead && bf.OID == hf.OID && bf.Mode == hf.Mode {
			continue
		}
		var bData, hData []byte
		if inBase {
			if obj := r.Objects[bf.OID]; obj != nil {
				bData = obj.Data
			}
		}
		if inHead {
			if obj := r.Objects[hf.OID]; obj != nil {
				hData = obj.Data
			}
		}
		if !inBase && inHead {
			adds := countLines(hData)
			if adds == 0 {
				adds = 1
			}
			diffs = append(diffs, FileDiff{
				Filename:  p,
				Status:    "added",
				Additions: adds,
				Deletions: 0,
				Changes:   adds,
			})
		} else if inBase && !inHead {
			dels := countLines(bData)
			if dels == 0 {
				dels = 1
			}
			diffs = append(diffs, FileDiff{
				Filename:  p,
				Status:    "removed",
				Additions: 0,
				Deletions: dels,
				Changes:   dels,
			})
		} else {
			adds, dels := diffLineCounts(bData, hData)
			if adds == 0 && dels == 0 {
				adds = 1
				dels = 1
			}
			diffs = append(diffs, FileDiff{
				Filename:  p,
				Status:    "modified",
				Additions: adds,
				Deletions: dels,
				Changes:   adds + dels,
			})
		}
	}
	return diffs, nil
}

func diffLineCounts(a, b []byte) (adds, dels int) {
	al := strings.Split(strings.TrimSuffix(string(a), "\n"), "\n")
	bl := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(a) == 0 {
		al = nil
	}
	if len(b) == 0 {
		bl = nil
	}
	m, n := len(al), len(bl)
	if m*n > 250000 {
		// Fallback for very large files
		return maxInt(1, n), maxInt(1, m)
	}
	dp := make([]int, (m+1)*(n+1))
	stride := n + 1
	for i := m - 1; i >= 0; i-- {
		for j := n - 1; j >= 0; j-- {
			if al[i] == bl[j] {
				dp[i*stride+j] = dp[(i+1)*stride+(j+1)] + 1
			} else if dp[(i+1)*stride+j] >= dp[i*stride+(j+1)] {
				dp[i*stride+j] = dp[(i+1)*stride+j]
			} else {
				dp[i*stride+j] = dp[i*stride+(j+1)]
			}
		}
	}
	lcs := dp[0]
	return n - lcs, m - lcs
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// MergeCommitsLocked performs a 3-way tree merge between baseCommit and headCommit.
// Returns (mergedTreeOID, hasConflict, error).
func MergeCommitsLocked(r *store.Repo, baseCommit, headCommit [20]byte) ([20]byte, bool, error) {
	lca, hasLCA := MergeBaseLocked(r, baseCommit, headCommit)
	var lcaFiles map[string]FlatEntry
	if hasLCA {
		var err error
		lcaFiles, err = FlattenCommitTreeLocked(r, lca)
		if err != nil {
			return ZeroOID, false, err
		}
	} else {
		lcaFiles = make(map[string]FlatEntry)
	}
	baseFiles, err := FlattenCommitTreeLocked(r, baseCommit)
	if err != nil {
		return ZeroOID, false, err
	}
	headFiles, err := FlattenCommitTreeLocked(r, headCommit)
	if err != nil {
		return ZeroOID, false, err
	}

	allPaths := make(map[string]struct{}, len(lcaFiles)+len(baseFiles)+len(headFiles))
	for p := range lcaFiles {
		allPaths[p] = struct{}{}
	}
	for p := range baseFiles {
		allPaths[p] = struct{}{}
	}
	for p := range headFiles {
		allPaths[p] = struct{}{}
	}

	merged := make(map[string]FlatEntry, len(allPaths))
	for p := range allPaths {
		le, inL := lcaFiles[p]
		be, inB := baseFiles[p]
		he, inH := headFiles[p]

		// Identical in base and head
		if inB == inH && (!inB || (be.OID == he.OID && be.Mode == he.Mode)) {
			if inB {
				merged[p] = be
			}
			continue
		}
		// Base unchanged since LCA -> take head
		if inB == inL && (!inB || (be.OID == le.OID && be.Mode == le.Mode)) {
			if inH {
				merged[p] = he
			}
			continue
		}
		// Head unchanged since LCA -> take base
		if inH == inL && (!inH || (he.OID == le.OID && he.Mode == le.Mode)) {
			if inB {
				merged[p] = be
			}
			continue
		}
		// Both sides diverged on the same path -> conflict
		return ZeroOID, true, nil
	}

	treeOID, err := WriteTreeFromFlatLocked(r, merged)
	if err != nil {
		return ZeroOID, false, err
	}
	return treeOID, false, nil
}
