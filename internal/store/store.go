package store

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
	Tree      [20]byte
	Parents   [][20]byte
	Author    string
	Committer string
	Message   string
	Timestamp int64
	Height    uint32
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

	GitMu   sync.RWMutex
	Objects map[[20]byte]*GitObject
	Commits map[[20]byte]*CommitInfo
	Refs    map[string][20]byte
	HEAD    string // e.g. "refs/heads/main"

	// Lock-free atomic HTTP response caches for Git Smart HTTP hot paths.
	CachedUploadV2InfoRefs []byte
	CachedRecvInfoRefs     atomic.Pointer[[]byte]
	CachedLsRefsDefault    atomic.Pointer[[]byte]
	CachedShallowClone     atomic.Pointer[ShallowCache]

	DataMu      sync.RWMutex
	NextNumber  int64
	Labels      map[string]*Label
	Issues      []*Issue
	IssuesByNum map[int64]*Issue
	FastIssues  atomic.Pointer[[]*Issue]
	Pulls       []*PullRequest
	PullsByNum  map[int64]*PullRequest
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
	var b strings.Builder
	b.WriteString(kind)
	for _, p := range parts {
		b.WriteByte(':')
		b.WriteString(strconv.FormatInt(p, 10))
	}
	return base64.RawURLEncoding.EncodeToString([]byte(b.String()))
}

var (
	cachedTimeSec atomic.Int64
	cachedTimeStr atomic.Pointer[string]
)

func nowISO() string {
	sec := time.Now().Unix()
	if cachedTimeSec.Load() == sec {
		if p := cachedTimeStr.Load(); p != nil {
			return *p
		}
	}
	s := time.Unix(sec, 0).UTC().Format("2006-01-02T15:04:05Z")
	cachedTimeStr.Store(&s)
	cachedTimeSec.Store(sec)
	return s
}

func repoKey(owner, name string) string {
	return strings.ToLower(owner) + "/" + strings.ToLower(name)
}

// GetRepo performs a lock-free repository lookup.
func (s *Store) GetRepo(owner, name string) *Repo {
	m := *s.reposMap.Load()
	return m[repoKey(owner, name)]
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
	if iss == nil {
		return nil, nil
	}
	return s.GetRepo(iss.RepoOwner, iss.RepoName), iss
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
	now := nowISO()

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
		Objects:                make(map[[20]byte]*GitObject),
		Commits:                make(map[[20]byte]*CommitInfo),
		Refs:                   make(map[string][20]byte),
		HEAD:                   "refs/heads/" + defaultBranch,
		Labels:                 make(map[string]*Label),
		IssuesByNum:            make(map[int64]*Issue),
		PullsByNum:             make(map[int64]*PullRequest),
		CachedUploadV2InfoRefs: buildUploadPackV2Advertisement(),
	}
	fastInit := make([]*Issue, 1, 16)
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
	newMap := make(map[string]*Repo, len(oldMap)-1)
	for k, v := range oldMap {
		if k != key {
			newMap[k] = v
		}
	}
	s.reposMap.Store(&newMap)
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

// CreateIssue creates a new issue in r, validating that all labelNames exist.
func (s *Store) CreateIssue(r *Repo, title, body string, labelNames []string) (*Issue, error) {
	r.DataMu.Lock()
	resolvedLabels := make([]*Label, 0, len(labelNames))
	for _, ln := range labelNames {
		lbl, ok := r.Labels[ln]
		if !ok {
			r.DataMu.Unlock()
			return nil, fmt.Errorf("unknown label %q", ln)
		}
		resolvedLabels = append(resolvedLabels, lbl)
	}
	r.NextNumber++
	num := r.NextNumber
	id := s.nextItemID.Add(1)
	gid := EncodeGlobalID("Issue", r.ID, num)
	now := nowISO()
	iss := &Issue{
		ID:          id,
		GlobalID:    gid,
		RepoOwner:   r.Owner,
		RepoName:    r.Name,
		Number:      num,
		Title:       title,
		Body:        body,
		State:       "open",
		Labels:      resolvedLabels,
		AuthorLogin: ViewerLogin,
		CreatedSeq:  s.seq.Add(1),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	r.Issues = append(r.Issues, iss)
	r.IssuesByNum[num] = iss

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
	r.DataMu.Unlock()

	s.mu.Lock()
	s.issuesByGID[gid] = iss
	s.mu.Unlock()

	return iss, nil
}

// UpdateIssue updates state/title/body/labels on an issue and refreshes its cached HTTP response.
func (s *Store) UpdateIssue(r *Repo, num int64, state, title, body *string, labelNames *[]string) (*Issue, error) {
	r.DataMu.Lock()
	defer r.DataMu.Unlock()
	iss, ok := r.IssuesByNum[num]
	if !ok {
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
	now := nowISO()
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
	iss, ok := r.IssuesByNum[num]
	if !ok {
		r.DataMu.Unlock()
		return nil
	}
	id := s.nextItemID.Add(1)
	gid := EncodeGlobalID("IssueComment", id)
	now := nowISO()
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
	now := nowISO()
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

// MarshalIssueJSON serializes an Issue into GitHub REST v3 JSON without reflection overhead.
func MarshalIssueJSON(iss *Issue, dst []byte) []byte {
	dst = append(dst, `{"id":`...)
	dst = strconv.AppendInt(dst, iss.ID, 10)
	dst = append(dst, `,"node_id":`...)
	dst = strconv.AppendQuote(dst, iss.GlobalID)
	dst = append(dst, `,"number":`...)
	dst = strconv.AppendInt(dst, iss.Number, 10)
	dst = append(dst, `,"title":`...)
	dst = strconv.AppendQuote(dst, iss.Title)
	dst = append(dst, `,"body":`...)
	dst = strconv.AppendQuote(dst, iss.Body)
	dst = append(dst, `,"state":`...)
	dst = strconv.AppendQuote(dst, iss.State)
	dst = append(dst, `,"user":{"id":1000,"login":"`...)
	dst = append(dst, ViewerLogin...)
	dst = append(dst, `"},"labels":[`...)
	for i, lbl := range iss.Labels {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, `{"id":`...)
		dst = strconv.AppendInt(dst, lbl.ID, 10)
		dst = append(dst, `,"name":`...)
		dst = strconv.AppendQuote(dst, lbl.Name)
		dst = append(dst, `,"color":`...)
		dst = strconv.AppendQuote(dst, lbl.Color)
		dst = append(dst, '}')
	}
	dst = append(dst, `],"comments":`...)
	dst = strconv.AppendInt(dst, int64(len(iss.Comments)), 10)
	dst = append(dst, `,"created_at":`...)
	dst = strconv.AppendQuote(dst, iss.CreatedAt)
	dst = append(dst, `,"updated_at":`...)
	dst = strconv.AppendQuote(dst, iss.UpdatedAt)
	if iss.ClosedAt != "" {
		dst = append(dst, `,"closed_at":`...)
		dst = strconv.AppendQuote(dst, iss.ClosedAt)
	} else {
		dst = append(dst, `,"closed_at":null`...)
	}
	dst = append(dst, '}')
	return dst
}

// RebuildIssueCache pre-builds the complete raw HTTP/1.1 200 OK response for GET /issues/:number.
func RebuildIssueCache(iss *Issue) {
	body := MarshalIssueJSON(iss, make([]byte, 0, 512))
	body = append(body, '\n')
	raw := http1.BuildResponse(200, "application/json; charset=utf-8", nil, "cached-"+strconv.FormatInt(iss.ID, 10), body)
	iss.CachedHTTP.Store(&raw)
}

func pktLineStr(s string) string {
	return fmt.Sprintf("%04x%s", len(s)+4, s)
}

func buildUploadPackV2Advertisement() []byte {
	payload := pktLineStr("version 2\n") +
		pktLineStr("agent=gitforge/1.0\n") +
		pktLineStr("ls-refs=unborn\n") +
		pktLineStr("fetch=shallow wait-for-done\n") +
		pktLineStr("object-format=sha1\n") +
		"0000"
	return http1.BuildResponse(200, "application/x-git-upload-pack-advertisement", []http1.Header{
		{Name: "Cache-Control", Value: "no-cache"},
	}, "git-v2-adv", []byte(payload))
}
