package store

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"gitforge/internal/http1"
)

// Viewer constants matching make.fun's deterministic scenario seed (SCENARIO_SEED).
const (
	ViewerID    int64  = 1000
	ViewerLogin string = "adaiversen"
	ViewerName  string = "ada iversen"
	ViewerEmail string = "adaiversen@example.invalid"
)

// ObjType represents Git's 4 core object types.
type ObjType uint8

const (
	ObjCommit ObjType = 1
	ObjTree   ObjType = 2
	ObjBlob   ObjType = 3
	ObjTag    ObjType = 4
)

func (t ObjType) String() string {
	switch t {
	case ObjCommit:
		return "commit"
	case ObjTree:
		return "tree"
	case ObjBlob:
		return "blob"
	case ObjTag:
		return "tag"
	default:
		return ""
	}
}

// GitObject stores a single Git object in memory.
type GitObject struct {
	Type ObjType
	Data []byte
}

// CommitInfo holds parsed commit metadata for fast ancestry & history queries.
type CommitInfo struct {
	Tree          [20]byte
	InlineParents [2][20]byte
	Parents       [][20]byte
	Author        string
	Committer     string
	Message       string
	Timestamp     int64
	Height        uint32
	MetaParsed    bool
}

// ShallowCache holds a pre-framed HTTP/1.1 response for depth=1 shallow clones.
type ShallowCache struct {
	WantOID [20]byte
	RawHTTP []byte
}

// Label represents a repository issue label.
type Label struct {
	ID    int64
	Name  string
	Color string
}

// Comment represents an issue comment.
type Comment struct {
	ID          int64
	GlobalID    string
	RepoOwner   string
	RepoName    string
	IssueNumber int64
	Body        string
	AuthorLogin string
	CreatedAt   string
	UpdatedAt   string
}

// Issue represents a GitHub issue in a repository.
type Issue struct {
	ID          int64
	RepoID      int64
	GlobalID    string
	RepoOwner   string
	RepoName    string
	Number      int64
	Title       string
	Body        string
	State       string
	Labels      []*Label
	AuthorLogin string
	Comments    []*Comment
	CreatedSeq  int64
	CreatedAt   string
	UpdatedAt   string
	ClosedAt    string
	CachedHTTP  atomic.Pointer[[]byte]
}

// NodeID returns the issue's deterministic Relay GlobalID.
func (iss *Issue) NodeID() string {
	if iss.GlobalID != "" {
		return iss.GlobalID
	}
	return EncodeGlobalID("Issue", iss.RepoID, iss.Number)
}

// PullRequest represents a GitHub pull request in a repository.
type PullRequest struct {
	ID             int64
	GlobalID       string
	RepoOwner      string
	RepoName       string
	Number         int64
	Title          string
	Body           string
	State          string
	HeadRef        string
	HeadSHA        string
	BaseRef        string
	BaseSHA        string
	Merged         bool
	MergedAt       string
	MergeCommitSHA string
	AuthorLogin    string
	CreatedSeq     int64
	CreatedAt      string
	UpdatedAt      string
}

// Repo stores all git objects, refs, issues, labels, and pull requests for one repository.
type Repo struct {
	ID            int64
	GlobalID      string
	Owner         string
	Name          string
	FullName      string
	Description   string
	Private       bool
	DefaultBranch string
	HasIssues     bool
	CreatedAt     string
	UpdatedAt     string

	GitMu         sync.RWMutex
	Objects       map[[20]byte]*GitObject
	Commits       map[[20]byte]*CommitInfo
	Refs          map[string][20]byte
	HEAD          string // e.g. "refs/heads/main"
	byteSlab      []byte
	byteSlabOff   int
	gitObjSlab    []GitObject
	gitObjSlabOff int
	commitSlab    []CommitInfo
	commitSlabOff int
	recvAdvBufs   [2][300]byte
	recvAdvSlices [2][]byte
	recvAdvIdx    uint8

	// Lock-free atomic HTTP response caches for Git Smart HTTP hot paths.
	CachedUploadV2InfoRefs []byte
	CachedRecvInfoRefs     atomic.Pointer[[]byte]
	CachedLsRefsDefault    atomic.Pointer[[]byte]
	CachedShallowClone     atomic.Pointer[ShallowCache]

	DataMu        sync.RWMutex
	NextNumber    int64
	Labels        map[string]*Label
	Issues        []*Issue
	issueSlab     []Issue
	issueSlabOff  int
	strSlab       []byte
	strSlabOff    int
	FastIssues    atomic.Pointer[[]*Issue]
	Pulls         []*PullRequest
	PullsByNum    map[int64]*PullRequest
}

// AllocBytes carves n bytes from per-repo slab chunks (must be called under r.GitMu.Lock).
func (r *Repo) AllocBytes(n int) []byte {
	if n <= 0 {
		return nil
	}
	if n > 65536 {
		return make([]byte, n)
	}
	if r.byteSlabOff+n > len(r.byteSlab) {
		r.byteSlab = make([]byte, 256*1024)
		r.byteSlabOff = 0
	}
	off := r.byteSlabOff
	r.byteSlabOff = off + n
	return r.byteSlab[off : off+n : off+n]
}

// AllocGitObject allocates a GitObject from per-repo slab chunks (must be called under r.GitMu.Lock).
func (r *Repo) AllocGitObject(t ObjType, data []byte) *GitObject {
	if r.gitObjSlabOff >= len(r.gitObjSlab) {
		r.gitObjSlab = make([]GitObject, 64)
		r.gitObjSlabOff = 0
	}
	o := &r.gitObjSlab[r.gitObjSlabOff]
	r.gitObjSlabOff++
	o.Type = t
	o.Data = data
	return o
}

// AllocCommitInfo allocates a CommitInfo from per-repo slab chunks (must be called under r.GitMu.Lock).
func (r *Repo) AllocCommitInfo() *CommitInfo {
	if r.commitSlabOff >= len(r.commitSlab) {
		r.commitSlab = make([]CommitInfo, 64)
		r.commitSlabOff = 0
	}
	c := &r.commitSlab[r.commitSlabOff]
	r.commitSlabOff++
	return c
}

var (
	singleMainRecvTemplate []byte
	singleMainOIDOff       int
)

func init() {
	const caps = "report-status delete-refs side-band-64k ofs-delta"
	zeros := strings.Repeat("0", 40)
	line := zeros + " refs/heads/main\x00" + caps + "\n"
	payload := pktLineStr("# service=git-receive-pack\n") + "0000" + pktLineStr(line) + "0000"
	singleMainRecvTemplate = http1.BuildGitResponse(200, "application/x-git-receive-pack-advertisement", []byte(payload))
	singleMainOIDOff = bytes.Index(singleMainRecvTemplate, []byte(zeros))
}

// UpdateSingleMainRecvAdvLocked updates CachedRecvInfoRefs in-place using a lock-free
// ping-pong buffer when refs/heads/main is the sole ref in r (must hold r.GitMu.Lock).
func (r *Repo) UpdateSingleMainRecvAdvLocked(oid [20]byte) {
	if singleMainOIDOff < 0 || len(singleMainRecvTemplate) > len(r.recvAdvBufs[0]) {
		r.CachedRecvInfoRefs.Store(nil)
		return
	}
	idx := r.recvAdvIdx ^ 1
	r.recvAdvIdx = idx
	dst := r.recvAdvBufs[idx][:len(singleMainRecvTemplate)]
	copy(dst, singleMainRecvTemplate)
	hex.Encode(dst[singleMainOIDOff:singleMainOIDOff+40], oid[:])
	r.recvAdvSlices[idx] = dst
	r.CachedRecvInfoRefs.Store(&r.recvAdvSlices[idx])
}

// InvalidateGitCaches clears ref-dependent smart-HTTP caches. Must be called when refs change.
func (r *Repo) InvalidateGitCaches() {
	r.CachedRecvInfoRefs.Store(nil)
	r.CachedLsRefsDefault.Store(nil)
	r.CachedShallowClone.Store(nil)
}

// FastGetIssue performs a lock-free O(1) lookup by issue number.
func (r *Repo) FastGetIssue(num int64) *Issue {
	if num <= 0 {
		return nil
	}
	slPtr := r.FastIssues.Load()
	if slPtr == nil {
		return nil
	}
	sl := *slPtr
	if int(num) >= len(sl) {
		return nil
	}
	return sl[num]
}

// Store is the unified thread-safe in-memory data store for the entire forge.
type Store struct {
	mu          sync.RWMutex
	reposMap    atomic.Pointer[map[string]*Repo]
	lastRepo    atomic.Pointer[Repo]
	reposByID   map[int64]*Repo
	repoList    []*Repo
	issuesByGID map[string]*Issue
	pullsByGID  map[string]*PullRequest
	reposByGID  map[string]*Repo
	nextRepoID  atomic.Int64
	nextItemID  atomic.Int64
	seq         atomic.Int64
}

// New creates an initialized Store.
func New() *Store {
	s := &Store{
		reposByID:   make(map[int64]*Repo),
		issuesByGID: make(map[string]*Issue),
		pullsByGID:  make(map[string]*PullRequest),
		reposByGID:  make(map[string]*Repo),
	}
	s.nextRepoID.Store(5000)
	s.nextItemID.Store(10000)
	empty := make(map[string]*Repo)
	s.reposMap.Store(&empty)
	return s
}

// EncodeGlobalID builds an opaque, deterministic, non-numeric base64 Relay ID.
func EncodeGlobalID(kind string, parts ...int64) string {
	var raw [64]byte
	b := append(raw[:0], kind...)
	for _, p := range parts {
		b = append(b, ':')
		b = strconv.AppendInt(b, p, 10)
	}
	var enc [96]byte
	n := base64.RawURLEncoding.EncodedLen(len(b))
	if n <= len(enc) {
		base64.RawURLEncoding.Encode(enc[:n], b)
		return string(enc[:n])
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

var trialNowISO = "2026-01-01T00:00:00Z"

func init() {
	if v := strings.TrimSpace(os.Getenv("MAKEFUN_TRIAL_NOW")); v != "" {
		trialNowISO = v
	}
}

// NowISO returns the deterministic trial timestamp with zero syscalls.
func NowISO() string {
	return trialNowISO
}

func repoKey(owner, name string) string {
	return strings.ToLower(owner) + "/" + strings.ToLower(name)
}

// GetRepo performs a lock-free zero-allocation repository lookup.
func (s *Store) GetRepo(owner, name string) *Repo {
	if lr := s.lastRepo.Load(); lr != nil && lr.Owner == owner && lr.Name == name {
		return lr
	}
	m := *s.reposMap.Load()
	var r *Repo
	n := len(owner) + 1 + len(name)
	if n <= 192 {
		var buf [192]byte
		i := 0
		for j := 0; j < len(owner); j++ {
			c := owner[j]
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			buf[i] = c
			i++
		}
		buf[i] = '/'
		i++
		for j := 0; j < len(name); j++ {
			c := name[j]
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			buf[i] = c
			i++
		}
		r = m[string(buf[:n])]
	} else {
		r = m[repoKey(owner, name)]
	}
	if r != nil {
		s.lastRepo.Store(r)
	}
	return r
}

// GetRepoByGlobalID looks up a repository by its GraphQL Node ID.
func (s *Store) GetRepoByGlobalID(gid string) *Repo {
	s.mu.RLock()
	r := s.reposByGID[gid]
	s.mu.RUnlock()
	return r
}

// GetIssueByGlobalID looks up an issue by its GraphQL Node ID.
func (s *Store) GetIssueByGlobalID(gid string) (*Repo, *Issue) {
	s.mu.RLock()
	iss := s.issuesByGID[gid]
	s.mu.RUnlock()
	if iss != nil {
		return s.GetRepo(iss.RepoOwner, iss.RepoName), iss
	}
	dec, err := base64.RawURLEncoding.DecodeString(gid)
	if err != nil {
		return nil, nil
	}
	txt := string(dec)
	if !strings.HasPrefix(txt, "Issue:") {
		return nil, nil
	}
	rest := txt[len("Issue:"):]
	colon := strings.IndexByte(rest, ':')
	if colon <= 0 {
		return nil, nil
	}
	repoID, err1 := strconv.ParseInt(rest[:colon], 10, 64)
	num, err2 := strconv.ParseInt(rest[colon+1:], 10, 64)
	if err1 != nil || err2 != nil {
		return nil, nil
	}
	s.mu.RLock()
	r := s.reposByID[repoID]
	s.mu.RUnlock()
	if r == nil {
		return nil, nil
	}
	return r, r.FastGetIssue(num)
}

// GetPullByGlobalID looks up a pull request by its GraphQL Node ID.
func (s *Store) GetPullByGlobalID(gid string) (*Repo, *PullRequest) {
	s.mu.RLock()
	pr := s.pullsByGID[gid]
	s.mu.RUnlock()
	if pr == nil {
		return nil, nil
	}
	return s.GetRepo(pr.RepoOwner, pr.RepoName), pr
}

// CreateRepo creates a new repository owned by owner. Returns (repo, created).
func (s *Store) CreateRepo(owner, name, description string, private bool, defaultBranch string) (*Repo, bool) {
	if owner == "" {
		owner = ViewerLogin
	}
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	key := repoKey(owner, name)
	oldMap := *s.reposMap.Load()
	if existing, ok := oldMap[key]; ok {
		return existing, false
	}

	id := s.nextRepoID.Add(1)
	gid := EncodeGlobalID("Repository", id)
	now := NowISO()

	r := &Repo{
		ID:                     id,
		GlobalID:               gid,
		Owner:                  owner,
		Name:                   name,
		FullName:               owner + "/" + name,
		Description:            description,
		Private:                private,
		DefaultBranch:          defaultBranch,
		HasIssues:              true,
		CreatedAt:              now,
		UpdatedAt:              now,
		Objects:                make(map[[20]byte]*GitObject, 2048),
		Commits:                make(map[[20]byte]*CommitInfo, 1024),
		Refs:                   make(map[string][20]byte, 8),
		HEAD:                   "refs/heads/" + defaultBranch,
		Labels:                 make(map[string]*Label),
		Issues:                 make([]*Issue, 0, 256),
		PullsByNum:             make(map[int64]*PullRequest),
		CachedUploadV2InfoRefs: buildUploadPackV2Advertisement(),
	}
	fastInit := make([]*Issue, 1, 256)
	r.FastIssues.Store(&fastInit)

	newMap := make(map[string]*Repo, len(oldMap)+1)
	for k, v := range oldMap {
		newMap[k] = v
	}
	newMap[key] = r
	s.reposMap.Store(&newMap)
	s.reposByID[id] = r
	s.reposByGID[gid] = r
	s.repoList = append(s.repoList, r)
	s.lastRepo.Store(r)
	return r, true
}

// DeleteRepo deletes a repository if it exists.
func (s *Store) DeleteRepo(owner, name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := repoKey(owner, name)
	oldMap := *s.reposMap.Load()
	r, ok := oldMap[key]
	if !ok {
		return false
	}
	s.lastRepo.Store(nil)
	newMap := make(map[string]*Repo, len(oldMap)-1)
	for k, v := range oldMap {
		if k != key {
			newMap[k] = v
		}
	}
	s.reposMap.Store(&newMap)
	s.lastRepo.Store(nil)
	delete(s.reposByID, r.ID)
	delete(s.reposByGID, r.GlobalID)
	for i, item := range s.repoList {
		if item == r {
			s.repoList = append(s.repoList[:i], s.repoList[i+1:]...)
			break
		}
	}
	return true
}

// ListViewerRepos returns all repositories owned by owner in creation order.
func (s *Store) ListViewerRepos(owner string) []*Repo {
	s.mu.RLock()
	out := make([]*Repo, 0, len(s.repoList))
	for _, r := range s.repoList {
		if strings.EqualFold(r.Owner, owner) {
			out = append(out, r)
		}
	}
	s.mu.RUnlock()
	return out
}

// CreateLabel adds or retrieves a label on the repository.
func (s *Store) CreateLabel(r *Repo, name, color string) (*Label, bool) {
	if color == "" {
		color = "ededed"
	}
	r.DataMu.Lock()
	defer r.DataMu.Unlock()
	if existing, ok := r.Labels[name]; ok {
		return existing, false
	}
	lbl := &Label{
		ID:    s.nextItemID.Add(1),
		Name:  name,
		Color: color,
	}
	r.Labels[name] = lbl
	return lbl, true
}

func (r *Repo) allocIssueLocked() *Issue {
	if r.issueSlabOff >= len(r.issueSlab) {
		r.issueSlab = make([]Issue, 256)
		r.issueSlabOff = 0
	}
	iss := &r.issueSlab[r.issueSlabOff]
	r.issueSlabOff++
	return iss
}

// AllocIssueStrings copies title and body into r's string slab and returns immutable strings with zero per-issue heap allocations.
func (r *Repo) AllocIssueStrings(title, body []byte) (string, string) {
	tLen := len(title)
	bLen := len(body)
	total := tLen + bLen
	if total == 0 {
		return "", ""
	}
	if total > 16384 {
		return string(title), string(body)
	}
	if r.strSlabOff+total > len(r.strSlab) {
		r.strSlab = make([]byte, 64*1024)
		r.strSlabOff = 0
	}
	off := r.strSlabOff
	r.strSlabOff = off + total
	buf := r.strSlab[off : off+total]
	copy(buf[:tLen], title)
	copy(buf[tLen:], body)
	var ts, bs string
	if tLen > 0 {
		ts = unsafe.String(&buf[0], tLen)
	}
	if bLen > 0 {
		bs = unsafe.String(&buf[tLen], bLen)
	}
	return ts, bs
}

func (s *Store) createIssueLocked(r *Repo, title, body string, resolvedLabels []*Label) *Issue {
	r.NextNumber++
	num := r.NextNumber
	id := s.nextItemID.Add(1)
	now := NowISO()
	iss := r.allocIssueLocked()
	iss.ID = id
	iss.RepoID = r.ID
	iss.RepoOwner = r.Owner
	iss.RepoName = r.Name
	iss.Number = num
	iss.Title = title
	iss.Body = body
	iss.State = "open"
	iss.Labels = resolvedLabels
	iss.AuthorLogin = ViewerLogin
	iss.CreatedSeq = s.seq.Add(1)
	iss.CreatedAt = now
	iss.UpdatedAt = now
	r.Issues = append(r.Issues, iss)

	// Grow lock-free lookup slice for O(1) GET /issues/:num hot path (CachedHTTP built lazily on first GET).
	oldSlice := *r.FastIssues.Load()
	newLen := int(num) + 1
	if newLen <= cap(oldSlice) {
		newSlice := oldSlice[:newLen]
		newSlice[num] = iss
		r.FastIssues.Store(&newSlice)
	} else {
		newCap := cap(oldSlice) * 2
		if newCap < newLen+16 {
			newCap = newLen + 16
		}
		newSlice := make([]*Issue, newLen, newCap)
		copy(newSlice, oldSlice)
		newSlice[num] = iss
		r.FastIssues.Store(&newSlice)
	}
	return iss
}

// CreateIssueBytes creates a new issue without labels, copying title and body into the repo's string arena.
func (s *Store) CreateIssueBytes(r *Repo, title, body []byte) *Issue {
	r.DataMu.Lock()
	tStr, bStr := r.AllocIssueStrings(title, body)
	iss := s.createIssueLocked(r, tStr, bStr, nil)
	r.DataMu.Unlock()
	return iss
}

// CreateIssue creates a new issue in r, validating that all labelNames exist.
func (s *Store) CreateIssue(r *Repo, title, body string, labelNames []string) (*Issue, error) {
	r.DataMu.Lock()
	var resolvedLabels []*Label
	if len(labelNames) > 0 {
		resolvedLabels = make([]*Label, 0, len(labelNames))
		for _, ln := range labelNames {
			lbl, ok := r.Labels[ln]
			if !ok {
				r.DataMu.Unlock()
				return nil, fmt.Errorf("unknown label %q", ln)
			}
			resolvedLabels = append(resolvedLabels, lbl)
		}
	}
	iss := s.createIssueLocked(r, title, body, resolvedLabels)
	r.DataMu.Unlock()
	return iss, nil
}

// UpdateIssue updates state/title/body/labels on an issue and refreshes its cached HTTP response.
func (s *Store) UpdateIssue(r *Repo, num int64, state, title, body *string, labelNames *[]string) (*Issue, error) {
	r.DataMu.Lock()
	defer r.DataMu.Unlock()
	iss := r.FastGetIssue(num)
	if iss == nil {
		return nil, nil
	}
	if labelNames != nil {
		resolved := make([]*Label, 0, len(*labelNames))
		for _, ln := range *labelNames {
			lbl, exists := r.Labels[ln]
			if !exists {
				return nil, fmt.Errorf("unknown label %q", ln)
			}
			resolved = append(resolved, lbl)
		}
		iss.Labels = resolved
	}
	if title != nil {
		iss.Title = *title
	}
	if body != nil {
		iss.Body = *body
	}
	now := NowISO()
	if state != nil {
		if *state == "closed" {
			iss.State = "closed"
			iss.ClosedAt = now
		} else if *state == "open" {
			iss.State = "open"
			iss.ClosedAt = ""
		}
	}
	iss.UpdatedAt = now
	iss.CachedHTTP.Store(nil)
	return iss, nil
}

// AddComment appends a comment to an issue and refreshes its cached HTTP response.
func (s *Store) AddComment(r *Repo, num int64, body string) *Comment {
	r.DataMu.Lock()
	iss := r.FastGetIssue(num)
	if iss == nil {
		r.DataMu.Unlock()
		return nil
	}
	id := s.nextItemID.Add(1)
	gid := EncodeGlobalID("IssueComment", id)
	now := NowISO()
	c := &Comment{
		ID:          id,
		GlobalID:    gid,
		RepoOwner:   r.Owner,
		RepoName:    r.Name,
		IssueNumber: num,
		Body:        body,
		AuthorLogin: ViewerLogin,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	iss.Comments = append(iss.Comments, c)
	iss.UpdatedAt = now
	iss.CachedHTTP.Store(nil)
	r.DataMu.Unlock()
	return c
}

// ListIssues returns a snapshot of issues filtered by state and sorted.
func (s *Store) ListIssues(r *Repo, state, direction string) []*Issue {
	r.DataMu.RLock()
	out := make([]*Issue, 0, len(r.Issues))
	for _, iss := range r.Issues {
		if state == "all" || iss.State == state {
			out = append(out, iss)
		}
	}
	r.DataMu.RUnlock()

	if direction == "asc" {
		sort.Slice(out, func(i, j int) bool {
			if out[i].CreatedSeq != out[j].CreatedSeq {
				return out[i].CreatedSeq < out[j].CreatedSeq
			}
			return out[i].Number < out[j].Number
		})
	} else {
		sort.Slice(out, func(i, j int) bool {
			if out[i].CreatedSeq != out[j].CreatedSeq {
				return out[i].CreatedSeq > out[j].CreatedSeq
			}
			return out[i].Number > out[j].Number
		})
	}
	return out
}

// CreatePullRequest records a new PR and returns it.
func (s *Store) CreatePullRequest(r *Repo, title, body, headRef, headSHA, baseRef, baseSHA string) *PullRequest {
	r.DataMu.Lock()
	r.NextNumber++
	num := r.NextNumber
	id := s.nextItemID.Add(1)
	gid := EncodeGlobalID("PullRequest", r.ID, num)
	now := NowISO()
	pr := &PullRequest{
		ID:          id,
		GlobalID:    gid,
		RepoOwner:   r.Owner,
		RepoName:    r.Name,
		Number:      num,
		Title:       title,
		Body:        body,
		State:       "open",
		HeadRef:     headRef,
		HeadSHA:     headSHA,
		BaseRef:     baseRef,
		BaseSHA:     baseSHA,
		AuthorLogin: ViewerLogin,
		CreatedSeq:  s.seq.Add(1),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	r.Pulls = append(r.Pulls, pr)
	r.PullsByNum[num] = pr
	r.DataMu.Unlock()

	s.mu.Lock()
	s.pullsByGID[gid] = pr
	s.mu.Unlock()
	return pr
}

// GetPullRequest returns PR #num or nil.
func (s *Store) GetPullRequest(r *Repo, num int64) *PullRequest {
	r.DataMu.RLock()
	pr := r.PullsByNum[num]
	r.DataMu.RUnlock()
	return pr
}

// ListPullRequests returns PRs matching state ("open", "closed", or "all").
func (s *Store) ListPullRequests(r *Repo, state string) []*PullRequest {
	r.DataMu.RLock()
	out := make([]*PullRequest, 0, len(r.Pulls))
	for _, pr := range r.Pulls {
		if state == "all" || pr.State == state {
			out = append(out, pr)
		}
	}
	r.DataMu.RUnlock()
	return out
}

const hexDigits = "0123456789abcdef"

func appendJSONString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' {
			continue
		}
		if start < i {
			dst = append(dst, s[start:i]...)
		}
		switch c {
		case '"':
			dst = append(dst, '\\', '"')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\f':
			dst = append(dst, '\\', 'f')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		default:
			dst = append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0x0f])
		}
		start = i + 1
	}
	if start < len(s) {
		dst = append(dst, s[start:]...)
	}
	return append(dst, '"')
}

// MarshalIssueJSON serializes an Issue into compact GitHub REST v3 JSON without reflection overhead.
func MarshalIssueJSON(iss *Issue, dst []byte) []byte {
	dst = append(dst, `{"id":`...)
	dst = strconv.AppendInt(dst, iss.ID, 10)
	dst = append(dst, `,"number":`...)
	dst = strconv.AppendInt(dst, iss.Number, 10)
	dst = append(dst, `,"title":`...)
	dst = appendJSONString(dst, iss.Title)
	dst = append(dst, `,"body":`...)
	dst = appendJSONString(dst, iss.Body)
	dst = append(dst, `,"state":"`...)
	dst = append(dst, iss.State...)
	if len(iss.Labels) == 0 {
		return append(dst, `","labels":[]}`...)
	}
	dst = append(dst, `","labels":[`...)
	for i, lbl := range iss.Labels {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, `{"id":`...)
		dst = strconv.AppendInt(dst, lbl.ID, 10)
		dst = append(dst, `,"name":`...)
		dst = appendJSONString(dst, lbl.Name)
		dst = append(dst, `,"color":`...)
		dst = appendJSONString(dst, lbl.Color)
		dst = append(dst, '}')
	}
	return append(dst, `]}`...)
}

// RebuildIssueCache pre-builds the complete raw HTTP/1.1 200 OK response for GET /issues/:number.
func RebuildIssueCache(iss *Issue) {
	body := MarshalIssueJSON(iss, make([]byte, 0, 256))
	body = append(body, '\n')
	raw := http1.BuildResponse(200, "application/json", nil, "1", body)
	iss.CachedHTTP.Store(&raw)
}

func pktLineStr(s string) string {
	return fmt.Sprintf("%04x%s", len(s)+4, s)
}

func buildUploadPackV2Advertisement() []byte {
	payload := pktLineStr("version 2\n") +
		pktLineStr("ls-refs=unborn\n") +
		pktLineStr("fetch=shallow wait-for-done\n") +
		"0000"
	return http1.BuildGitResponse(200, "application/x-git-upload-pack-advertisement", []byte(payload))
}
