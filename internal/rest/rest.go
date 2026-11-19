package rest

import (
	"encoding/base64"
	"encoding/json"
	"path"
	"strconv"
	"strings"
	"time"

	"gitforge/internal/git"
	"gitforge/internal/http1"
	"gitforge/internal/store"
)

// HandleREST dispatches all /api/v3/... requests. Authentication is already verified by caller.
func HandleREST(w *http1.ResponseWriter, r *http1.Request, st *store.Store) {
	p := r.Path
	// Hot path check first: GET /api/v3/repos/<owner>/<repo>/issues/<number>
	if r.Method == "GET" && strings.HasPrefix(p, "/api/v3/repos/") {
		rest := p[len("/api/v3/repos/"):]
		if idx := strings.Index(rest, "/issues/"); idx > 0 {
			numPart := rest[idx+len("/issues/"):]
			if len(numPart) > 0 && !strings.ContainsRune(numPart, '/') {
				if num, ok := parseFastInt64(numPart); ok {
					ownerRepo := rest[:idx]
					if slash := strings.IndexByte(ownerRepo, '/'); slash > 0 && !strings.ContainsRune(ownerRepo[slash+1:], '/') {
						owner := ownerRepo[:slash]
						repoName := ownerRepo[slash+1:]
						repo := st.GetRepo(owner, repoName)
						if repo == nil {
							writeErr(w, 404, "Not Found")
							return
						}
						iss := repo.FastGetIssue(num)
						if iss == nil {
							writeErr(w, 404, "Not Found")
							return
						}
						if raw := iss.CachedHTTP.Load(); raw != nil {
							_ = w.WriteRaw(*raw)
							return
						}
						repo.DataMu.RLock()
						store.RebuildIssueCache(iss)
						rawPtr := iss.CachedHTTP.Load()
						repo.DataMu.RUnlock()
						if rawPtr != nil {
							_ = w.WriteRaw(*rawPtr)
							return
						}
					}
				}
			}
		}
	}

	if p == "/api/v3" || p == "/api/v3/" {
		if r.Method == "GET" {
			_ = w.WriteJSONWithHeaders(200, []http1.Header{
				{Name: "X-OAuth-Scopes", Value: "repo, read:org"},
			}, []byte(`{"current_user_url":"/api/v3/user","rate_limit_url":"/api/v3/rate_limit"}`+"\n"))
			return
		}
		writeErr(w, 404, "Not Found")
		return
	}

	if p == "/api/v3/meta" && r.Method == "GET" {
		_ = w.WriteJSON(200, []byte(`{"installed_version":"3.14.0","verifiable_password_authentication":false}`+"\n"))
		return
	}

	if p == "/api/v3/user" && r.Method == "GET" {
		b, _ := json.Marshal(map[string]any{
			"id":    store.ViewerID,
			"login": store.ViewerLogin,
			"name":  store.ViewerName,
			"email": store.ViewerEmail,
		})
		_ = w.WriteJSON(200, append(b, '\n'))
		return
	}

	if p == "/api/v3/user/repos" {
		switch r.Method {
		case "POST":
			handleCreateRepo(w, r, st)
		case "GET":
			handleListUserRepos(w, r, st)
		default:
			writeErr(w, 404, "Not Found")
		}
		return
	}

	if strings.HasPrefix(p, "/api/v3/repos/") {
		tail := p[len("/api/v3/repos/"):]
		s1 := strings.IndexByte(tail, '/')
		if s1 <= 0 {
			writeErr(w, 404, "Not Found")
			return
		}
		owner := tail[:s1]
		rem := tail[s1+1:]
		var repoName, sub string
		if s2 := strings.IndexByte(rem, '/'); s2 < 0 {
			repoName = rem
			sub = ""
		} else {
			repoName = rem[:s2]
			sub = rem[s2:]
		}
		if repoName == "" {
			writeErr(w, 404, "Not Found")
			return
		}
		repo := st.GetRepo(owner, repoName)
		if repo == nil {
			writeErr(w, 404, "Not Found")
			return
		}
		handleRepoSub(w, r, st, repo, sub)
		return
	}

	writeErr(w, 404, "Not Found")
}

func parseFastInt64(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	var n int64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int64(c-'0')
	}
	return n, true
}

func writeErr(w *http1.ResponseWriter, status int, msg string) {
	b := make([]byte, 0, len(msg)+24)
	b = append(b, `{"message":`...)
	b = strconv.AppendQuote(b, msg)
	b = append(b, "}\n"...)
	_ = w.WriteJSON(status, b)
}

func writeJSONVal(w *http1.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		writeErr(w, 500, "JSON serialization error")
		return
	}
	_ = w.WriteJSON(status, append(b, '\n'))
}

func repoJSON(r *store.Repo) map[string]any {
	r.DataMu.RLock()
	desc := r.Description
	priv := r.Private
	defBranch := r.DefaultBranch
	r.DataMu.RUnlock()
	return map[string]any{
		"id":             r.ID,
		"node_id":        r.GlobalID,
		"name":           r.Name,
		"full_name":      r.FullName,
		"private":        priv,
		"description":    desc,
		"default_branch": defBranch,
		"has_issues":     r.HasIssues,
		"owner": map[string]any{
			"id":    store.ViewerID,
			"login": r.Owner,
		},
		"created_at": r.CreatedAt,
		"updated_at": r.UpdatedAt,
	}
}

func handleCreateRepo(w *http1.ResponseWriter, r *http1.Request, st *store.Store) {
	var req struct {
		Name          string `json:"name"`
		Description   string `json:"description"`
		Private       bool   `json:"private"`
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(r.Body, &req); err != nil {
		writeErr(w, 400, "Problems parsing JSON")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeErr(w, 422, "Repository creation failed: name is required")
		return
	}
	repo, created := st.CreateRepo(store.ViewerLogin, req.Name, req.Description, req.Private, req.DefaultBranch)
	if !created {
		writeErr(w, 422, "Repository creation failed: name already exists on this account")
		return
	}
	writeJSONVal(w, 201, repoJSON(repo))
}

func handleListUserRepos(w *http1.ResponseWriter, r *http1.Request, st *store.Store) {
	perPage := 30
	if pp := r.QueryParam("per_page"); pp != "" {
		if n, err := strconv.Atoi(pp); err == nil && n > 0 {
			if n > 100 {
				n = 100
			}
			perPage = n
		}
	}
	page := 1
	if pg := r.QueryParam("page"); pg != "" {
		if n, err := strconv.Atoi(pg); err == nil && n > 0 {
			page = n
		}
	}
	all := st.ListViewerRepos(store.ViewerLogin)
	total := len(all)
	totalPages := (total + perPage - 1) / perPage
	if totalPages == 0 {
		totalPages = 1
	}

	start := (page - 1) * perPage
	var slice []*store.Repo
	if start < total {
		end := start + perPage
		if end > total {
			end = total
		}
		slice = all[start:end]
	}

	items := make([]map[string]any, 0, len(slice))
	for _, rp := range slice {
		items = append(items, repoJSON(rp))
	}

	var headers []http1.Header
	if link := buildLinkHeader(r, "/api/v3/user/repos", page, perPage, totalPages); link != "" {
		headers = append(headers, http1.Header{Name: "Link", Value: link})
	}
	b, _ := json.Marshal(items)
	_ = w.WriteJSONWithHeaders(200, headers, append(b, '\n'))
}

func buildLinkHeader(r *http1.Request, basePath string, page, perPage, totalPages int) string {
	if totalPages <= 1 {
		return ""
	}
	host := r.HeaderValue("Host")
	if host == "" {
		host = "127.0.0.1"
	}
	mkURL := func(p int) string {
		return "<http://" + host + basePath + "?per_page=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(p) + ">"
	}
	var parts []string
	if page < totalPages {
		parts = append(parts, mkURL(page+1)+`; rel="next"`)
		parts = append(parts, mkURL(totalPages)+`; rel="last"`)
	}
	if page > 1 {
		parts = append(parts, mkURL(1)+`; rel="first"`)
		parts = append(parts, mkURL(page-1)+`; rel="prev"`)
	}
	return strings.Join(parts, ", ")
}

func handleRepoSub(w *http1.ResponseWriter, r *http1.Request, st *store.Store, repo *store.Repo, sub string) {
	switch {
	case sub == "" || sub == "/":
		switch r.Method {
		case "GET":
			writeJSONVal(w, 200, repoJSON(repo))
		case "PATCH":
			var req struct {
				Description   *string `json:"description"`
				Private       *bool   `json:"private"`
				DefaultBranch *string `json:"default_branch"`
			}
			if err := json.Unmarshal(r.Body, &req); err != nil {
				writeErr(w, 400, "Problems parsing JSON")
				return
			}
			repo.DataMu.Lock()
			if req.Description != nil {
				repo.Description = *req.Description
			}
			if req.Private != nil {
				repo.Private = *req.Private
			}
			if req.DefaultBranch != nil && *req.DefaultBranch != "" {
				repo.DefaultBranch = *req.DefaultBranch
				repo.GitMu.Lock()
				repo.HEAD = "refs/heads/" + *req.DefaultBranch
				repo.InvalidateGitCaches()
				repo.GitMu.Unlock()
			}
			repo.UpdatedAt = time.Now().UTC().Format("2006-01-02T15:04:05Z")
			repo.DataMu.Unlock()
			writeJSONVal(w, 200, repoJSON(repo))
		case "DELETE":
			st.DeleteRepo(repo.Owner, repo.Name)
			_ = w.WriteResponse(204, "", nil, nil)
		default:
			writeErr(w, 404, "Not Found")
		}
		return

	case sub == "/branches" && r.Method == "GET":
		handleListBranches(w, repo)
		return

	case strings.HasPrefix(sub, "/branches/") && r.Method == "GET":
		branch := sub[len("/branches/"):]
		repo.GitMu.RLock()
		oid, ok := repo.Refs["refs/heads/"+branch]
		repo.GitMu.RUnlock()
		if !ok {
			writeErr(w, 404, "Branch not found")
			return
		}
		writeJSONVal(w, 200, map[string]any{
			"name":      branch,
			"commit":    map[string]any{"sha": git.FormatOID(oid)},
			"protected": false,
		})
		return

	case sub == "/git/refs" && r.Method == "POST":
		handleCreateGitRef(w, r, repo)
		return

	case strings.HasPrefix(sub, "/git/refs/") && r.Method == "GET":
		refName := "refs/" + sub[len("/git/refs/"):]
		repo.GitMu.RLock()
		oid, ok := repo.Refs[refName]
		repo.GitMu.RUnlock()
		if !ok {
			writeErr(w, 404, "Not Found")
			return
		}
		writeJSONVal(w, 200, map[string]any{
			"ref": refName,
			"object": map[string]any{
				"type": "commit",
				"sha":  git.FormatOID(oid),
			},
		})
		return

	case sub == "/commits" && r.Method == "GET":
		handleListCommits(w, r, repo)
		return

	case strings.HasPrefix(sub, "/commits/") && r.Method == "GET":
		shaStr := sub[len("/commits/"):]
		handleGetCommit(w, repo, shaStr)
		return

	case strings.HasPrefix(sub, "/contents/") || sub == "/contents":
		filePath := ""
		if len(sub) > len("/contents/") {
			filePath = sub[len("/contents/"):]
		}
		switch r.Method {
		case "GET":
			handleGetContents(w, r, repo, filePath)
		case "PUT":
			handlePutContents(w, r, repo, filePath)
		case "DELETE":
			handleDeleteContents(w, r, repo, filePath)
		default:
			writeErr(w, 404, "Not Found")
		}
		return

	case sub == "/labels":
		switch r.Method {
		case "POST":
			handleCreateLabel(w, r, st, repo)
		case "GET":
			repo.DataMu.RLock()
			list := make([]map[string]any, 0, len(repo.Labels))
			for _, l := range repo.Labels {
				list = append(list, map[string]any{"id": l.ID, "name": l.Name, "color": l.Color})
			}
			repo.DataMu.RUnlock()
			writeJSONVal(w, 200, list)
		default:
			writeErr(w, 404, "Not Found")
		}
		return

	case sub == "/issues":
		switch r.Method {
		case "POST":
			handleCreateIssue(w, r, st, repo)
		case "GET":
			handleListIssues(w, r, st, repo)
		default:
			writeErr(w, 404, "Not Found")
		}
		return

	case strings.HasPrefix(sub, "/issues/"):
		handleIssuesSub(w, r, st, repo, sub[len("/issues/"):])
		return

	case sub == "/pulls":
		switch r.Method {
		case "POST":
			handleCreatePull(w, r, st, repo)
		case "GET":
			handleListPulls(w, r, st, repo)
		default:
			writeErr(w, 404, "Not Found")
		}
		return

	case strings.HasPrefix(sub, "/pulls/"):
		handlePullsSub(w, r, st, repo, sub[len("/pulls/"):])
		return
	}

	writeErr(w, 404, "Not Found")
}

func handleListBranches(w *http1.ResponseWriter, repo *store.Repo) {
	repo.GitMu.RLock()
	out := make([]map[string]any, 0, len(repo.Refs))
	for ref, oid := range repo.Refs {
		if strings.HasPrefix(ref, "refs/heads/") {
			out = append(out, map[string]any{
				"name":      ref[len("refs/heads/"):],
				"commit":    map[string]any{"sha": git.FormatOID(oid)},
				"protected": false,
			})
		}
	}
	repo.GitMu.RUnlock()
	writeJSONVal(w, 200, out)
}

func handleCreateGitRef(w *http1.ResponseWriter, r *http1.Request, repo *store.Repo) {
	var req struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(r.Body, &req); err != nil {
		writeErr(w, 400, "Problems parsing JSON")
		return
	}
	oid, ok := git.ParseOID(req.SHA)
	if !ok || !strings.HasPrefix(req.Ref, "refs/") {
		writeErr(w, 422, "Invalid ref or sha")
		return
	}
	repo.GitMu.Lock()
	if _, exists := repo.Objects[oid]; !exists {
		repo.GitMu.Unlock()
		writeErr(w, 422, "Object does not exist")
		return
	}
	if _, exists := repo.Refs[req.Ref]; exists {
		repo.GitMu.Unlock()
		writeErr(w, 422, "Reference already exists")
		return
	}
	repo.Refs[req.Ref] = oid
	repo.InvalidateGitCaches()
	repo.GitMu.Unlock()

	writeJSONVal(w, 201, map[string]any{
		"ref": req.Ref,
		"object": map[string]any{
			"type": "commit",
			"sha":  req.SHA,
		},
	})
}

func resolveRefLocked(repo *store.Repo, refOrSHA string) ([20]byte, bool) {
	if refOrSHA == "" {
		refOrSHA = repo.DefaultBranch
	}
	if oid, ok := git.ParseOID(refOrSHA); ok {
		if _, exists := repo.Commits[oid]; exists {
			return oid, true
		}
	}
	if oid, ok := repo.Refs[refOrSHA]; ok {
		return oid, true
	}
	if oid, ok := repo.Refs["refs/heads/"+refOrSHA]; ok {
		return oid, true
	}
	if oid, ok := repo.Refs["refs/tags/"+refOrSHA]; ok {
		if peeled, ok2 := git.PeelTagLocked(repo, oid); ok2 {
			return peeled, true
		}
		return oid, true
	}
	return git.ZeroOID, false
}

func handleListCommits(w *http1.ResponseWriter, r *http1.Request, repo *store.Repo) {
	shaParam := r.QueryParam("sha")
	repo.GitMu.RLock()
	tip, ok := resolveRefLocked(repo, shaParam)
	if !ok {
		repo.GitMu.RUnlock()
		writeJSONVal(w, 200, []any{})
		return
	}
	seen := make(map[[20]byte]bool)
	queue := [][20]byte{tip}
	var out []map[string]any
	for len(queue) > 0 && len(out) < 200 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		ci := repo.Commits[cur]
		if ci == nil {
			continue
		}
		out = append(out, map[string]any{
			"sha": git.FormatOID(cur),
			"commit": map[string]any{
				"message": strings.TrimSuffix(ci.Message, "\n"),
			},
		})
		for _, p := range ci.Parents {
			if !seen[p] {
				queue = append(queue, p)
			}
		}
	}
	repo.GitMu.RUnlock()
	writeJSONVal(w, 200, out)
}

func handleGetCommit(w *http1.ResponseWriter, repo *store.Repo, shaStr string) {
	repo.GitMu.RLock()
	oid, ok := resolveRefLocked(repo, shaStr)
	var ci *store.CommitInfo
	if ok {
		ci = repo.Commits[oid]
	}
	repo.GitMu.RUnlock()
	if !ok || ci == nil {
		writeErr(w, 404, "Not Found")
		return
	}
	writeJSONVal(w, 200, map[string]any{
		"sha": git.FormatOID(oid),
		"commit": map[string]any{
			"message": strings.TrimSuffix(ci.Message, "\n"),
		},
	})
}

func handleGetContents(w *http1.ResponseWriter, r *http1.Request, repo *store.Repo, filePath string) {
	cleanPath := strings.Trim(filePath, "/")
	ref := r.QueryParam("ref")

	repo.GitMu.RLock()
	commitOID, ok := resolveRefLocked(repo, ref)
	if !ok {
		repo.GitMu.RUnlock()
		writeErr(w, 404, "Not Found")
		return
	}
	ci := repo.Commits[commitOID]
	if ci == nil {
		repo.GitMu.RUnlock()
		writeErr(w, 404, "Not Found")
		return
	}

	curTreeOID := ci.Tree
	isDir := cleanPath == ""
	var blobOID [20]byte

	if !isDir {
		segs := strings.Split(cleanPath, "/")
		for i, seg := range segs {
			treeObj := repo.Objects[curTreeOID]
			if treeObj == nil || treeObj.Type != store.ObjTree {
				repo.GitMu.RUnlock()
				writeErr(w, 404, "Not Found")
				return
			}
			entries, err := git.ParseTree(treeObj.Data)
			if err != nil {
				repo.GitMu.RUnlock()
				writeErr(w, 404, "Not Found")
				return
			}
			var found *git.TreeEntry
			for j := range entries {
				if entries[j].Name == seg {
					found = &entries[j]
					break
				}
			}
			if found == nil {
				repo.GitMu.RUnlock()
				writeErr(w, 404, "Not Found")
				return
			}
			if i == len(segs)-1 {
				if found.Mode == "40000" {
					isDir = true
					curTreeOID = found.OID
				} else {
					blobOID = found.OID
				}
			} else {
				if found.Mode != "40000" {
					repo.GitMu.RUnlock()
					writeErr(w, 404, "Not Found")
					return
				}
				curTreeOID = found.OID
			}
		}
	}

	if isDir {
		treeObj := repo.Objects[curTreeOID]
		if treeObj == nil || treeObj.Type != store.ObjTree {
			repo.GitMu.RUnlock()
			writeErr(w, 404, "Not Found")
			return
		}
		entries, err := git.ParseTree(treeObj.Data)
		if err != nil {
			repo.GitMu.RUnlock()
			writeErr(w, 404, "Not Found")
			return
		}
		list := make([]map[string]any, 0, len(entries))
		for _, e := range entries {
			entryPath := e.Name
			if cleanPath != "" {
				entryPath = cleanPath + "/" + e.Name
			}
			kind := "file"
			size := 0
			if e.Mode == "40000" {
				kind = "dir"
			} else if bo := repo.Objects[e.OID]; bo != nil {
				size = len(bo.Data)
			}
			list = append(list, map[string]any{
				"type": kind,
				"name": e.Name,
				"path": entryPath,
				"sha":  git.FormatOID(e.OID),
				"size": size,
			})
		}
		repo.GitMu.RUnlock()
		writeJSONVal(w, 200, list)
		return
	}

	blobObj := repo.Objects[blobOID]
	if blobObj == nil || blobObj.Type != store.ObjBlob {
		repo.GitMu.RUnlock()
		writeErr(w, 404, "Not Found")
		return
	}
	blobCopy := make([]byte, len(blobObj.Data))
	copy(blobCopy, blobObj.Data)
	repo.GitMu.RUnlock()

	writeJSONVal(w, 200, map[string]any{
		"type":     "file",
		"encoding": "base64",
		"name":     path.Base(cleanPath),
		"path":     cleanPath,
		"sha":      git.FormatOID(blobOID),
		"size":     len(blobCopy),
		"content":  base64.StdEncoding.EncodeToString(blobCopy),
	})
}

func decodeGitHubBase64(s string) ([]byte, error) {
	cleaned := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, s)
	return base64.StdEncoding.DecodeString(cleaned)
}

func handlePutContents(w *http1.ResponseWriter, r *http1.Request, repo *store.Repo, filePath string) {
	cleanPath := strings.Trim(filePath, "/")
	if cleanPath == "" {
		writeErr(w, 422, "Invalid path")
		return
	}
	var req struct {
		Message string `json:"message"`
		Content string `json:"content"`
		Branch  string `json:"branch"`
		SHA     string `json:"sha"`
	}
	if err := json.Unmarshal(r.Body, &req); err != nil {
		writeErr(w, 400, "Problems parsing JSON")
		return
	}
	rawBytes, err := decodeGitHubBase64(req.Content)
	if err != nil {
		writeErr(w, 422, "Invalid base64 content")
		return
	}
	branch := req.Branch
	if branch == "" {
		branch = repo.DefaultBranch
	}
	refName := "refs/heads/" + branch

	repo.GitMu.Lock()
	defer repo.GitMu.Unlock()

	var parentOID [20]byte
	hasParent := false
	if existingRef, ok := repo.Refs[refName]; ok {
		parentOID = existingRef
		hasParent = true
	} else if defRef, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; ok {
		parentOID = defRef
		hasParent = true
	}

	var files map[string]git.FlatEntry
	if hasParent {
		files, err = git.FlattenCommitTreeLocked(repo, parentOID)
		if err != nil {
			writeErr(w, 500, "Failed to read tree")
			return
		}
	} else {
		files = make(map[string]git.FlatEntry)
	}

	existingEntry, fileExists := files[cleanPath]
	if req.SHA != "" {
		if !fileExists || git.FormatOID(existingEntry.OID) != strings.ToLower(req.SHA) {
			writeErr(w, 409, cleanPath+" does not match "+req.SHA)
			return
		}
	}

	blobOID, err := git.PutObjectLocked(repo, store.ObjBlob, rawBytes)
	if err != nil {
		writeErr(w, 500, "Failed to store blob")
		return
	}
	files[cleanPath] = git.FlatEntry{Mode: "100644", OID: blobOID}

	treeOID, err := git.WriteTreeFromFlatLocked(repo, files)
	if err != nil {
		writeErr(w, 500, "Failed to write tree")
		return
	}
	var parents [][20]byte
	if hasParent {
		parents = [][20]byte{parentOID}
	}
	ts := int64(1735689600 + len(repo.Commits))
	commitBytes := git.BuildCommit(treeOID, parents, store.ViewerName, store.ViewerEmail, ts, req.Message)
	commitOID, err := git.PutObjectLocked(repo, store.ObjCommit, commitBytes)
	if err != nil {
		writeErr(w, 500, "Failed to store commit")
		return
	}
	repo.Refs[refName] = commitOID
	repo.InvalidateGitCaches()

	status := 201
	if fileExists {
		status = 200
	}
	writeJSONVal(w, status, map[string]any{
		"content": map[string]any{
			"name":     path.Base(cleanPath),
			"path":     cleanPath,
			"sha":      git.FormatOID(blobOID),
			"size":     len(rawBytes),
			"type":     "file",
			"encoding": "base64",
		},
		"commit": map[string]any{
			"sha":     git.FormatOID(commitOID),
			"message": req.Message,
		},
	})
}

func handleDeleteContents(w *http1.ResponseWriter, r *http1.Request, repo *store.Repo, filePath string) {
	cleanPath := strings.Trim(filePath, "/")
	if cleanPath == "" {
		writeErr(w, 422, "Invalid path")
		return
	}
	var req struct {
		Message string `json:"message"`
		SHA     string `json:"sha"`
		Branch  string `json:"branch"`
	}
	if err := json.Unmarshal(r.Body, &req); err != nil {
		writeErr(w, 400, "Problems parsing JSON")
		return
	}
	branch := req.Branch
	if branch == "" {
		branch = repo.DefaultBranch
	}
	refName := "refs/heads/" + branch

	repo.GitMu.Lock()
	defer repo.GitMu.Unlock()

	parentOID, ok := repo.Refs[refName]
	if !ok {
		writeErr(w, 404, "Not Found")
		return
	}
	files, err := git.FlattenCommitTreeLocked(repo, parentOID)
	if err != nil {
		writeErr(w, 500, "Failed to read tree")
		return
	}
	existingEntry, exists := files[cleanPath]
	if !exists {
		writeErr(w, 404, "Not Found")
		return
	}
	if req.SHA != "" && git.FormatOID(existingEntry.OID) != strings.ToLower(req.SHA) {
		writeErr(w, 409, cleanPath+" does not match "+req.SHA)
		return
	}
	delete(files, cleanPath)

	treeOID, err := git.WriteTreeFromFlatLocked(repo, files)
	if err != nil {
		writeErr(w, 500, "Failed to write tree")
		return
	}
	ts := int64(1735689600 + len(repo.Commits))
	commitBytes := git.BuildCommit(treeOID, [][20]byte{parentOID}, store.ViewerName, store.ViewerEmail, ts, req.Message)
	commitOID, err := git.PutObjectLocked(repo, store.ObjCommit, commitBytes)
	if err != nil {
		writeErr(w, 500, "Failed to store commit")
		return
	}
	repo.Refs[refName] = commitOID
	repo.InvalidateGitCaches()

	writeJSONVal(w, 200, map[string]any{
		"content": nil,
		"commit": map[string]any{
			"sha":     git.FormatOID(commitOID),
			"message": req.Message,
		},
	})
}

func handleCreateLabel(w *http1.ResponseWriter, r *http1.Request, st *store.Store, repo *store.Repo) {
	var req struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	}
	if err := json.Unmarshal(r.Body, &req); err != nil {
		writeErr(w, 400, "Problems parsing JSON")
		return
	}
	if req.Name == "" {
		writeErr(w, 422, "Label name is required")
		return
	}
	lbl, _ := st.CreateLabel(repo, req.Name, req.Color)
	writeJSONVal(w, 201, map[string]any{
		"id":    lbl.ID,
		"name":  lbl.Name,
		"color": lbl.Color,
	})
}

func handleCreateIssue(w *http1.ResponseWriter, r *http1.Request, st *store.Store, repo *store.Repo) {
	var req struct {
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		Labels []string `json:"labels"`
	}
	if err := json.Unmarshal(r.Body, &req); err != nil {
		writeErr(w, 400, "Problems parsing JSON")
		return
	}
	iss, err := st.CreateIssue(repo, req.Title, req.Body, req.Labels)
	if err != nil {
		writeErr(w, 422, "Validation Failed: "+err.Error())
		return
	}
	buf := store.MarshalIssueJSON(iss, make([]byte, 0, 512))
	buf = append(buf, '\n')
	_ = w.WriteJSON(201, buf)
}

func handleListIssues(w *http1.ResponseWriter, r *http1.Request, st *store.Store, repo *store.Repo) {
	state := r.QueryParam("state")
	if state == "" {
		state = "open"
	}
	dir := r.QueryParam("direction")
	if dir == "" {
		dir = "desc"
	}
	perPage := 30
	if pp := r.QueryParam("per_page"); pp != "" {
		if n, err := strconv.Atoi(pp); err == nil && n > 0 {
			if n > 100 {
				n = 100
			}
			perPage = n
		}
	}
	page := 1
	if pg := r.QueryParam("page"); pg != "" {
		if n, err := strconv.Atoi(pg); err == nil && n > 0 {
			page = n
		}
	}

	all := st.ListIssues(repo, state, dir)
	total := len(all)
	start := (page - 1) * perPage
	var slice []*store.Issue
	if start < total {
		end := start + perPage
		if end > total {
			end = total
		}
		slice = all[start:end]
	}

	out := make([]byte, 0, 2+len(slice)*320)
	out = append(out, '[')
	for i, iss := range slice {
		if i > 0 {
			out = append(out, ',')
		}
		out = store.MarshalIssueJSON(iss, out)
	}
	out = append(out, ']', '\n')
	_ = w.WriteJSON(200, out)
}

func handleIssuesSub(w *http1.ResponseWriter, r *http1.Request, st *store.Store, repo *store.Repo, rest string) {
	var numStr, sub string
	if s := strings.IndexByte(rest, '/'); s < 0 {
		numStr = rest
	} else {
		numStr = rest[:s]
		sub = rest[s:]
	}
	num, ok := parseFastInt64(numStr)
	if !ok || num <= 0 {
		writeErr(w, 404, "Not Found")
		return
	}

	if sub == "" {
		switch r.Method {
		case "GET":
			iss := repo.FastGetIssue(num)
			if iss == nil {
				writeErr(w, 404, "Not Found")
				return
			}
			if raw := iss.CachedHTTP.Load(); raw != nil {
				_ = w.WriteRaw(*raw)
				return
			}
			repo.DataMu.RLock()
			store.RebuildIssueCache(iss)
			rawPtr := iss.CachedHTTP.Load()
			repo.DataMu.RUnlock()
			if rawPtr != nil {
				_ = w.WriteRaw(*rawPtr)
				return
			}
			buf := store.MarshalIssueJSON(iss, make([]byte, 0, 512))
			_ = w.WriteJSON(200, append(buf, '\n'))
		case "PATCH":
			var req struct {
				State  *string   `json:"state"`
				Title  *string   `json:"title"`
				Body   *string   `json:"body"`
				Labels *[]string `json:"labels"`
			}
			if err := json.Unmarshal(r.Body, &req); err != nil {
				writeErr(w, 400, "Problems parsing JSON")
				return
			}
			iss, err := st.UpdateIssue(repo, num, req.State, req.Title, req.Body, req.Labels)
			if err != nil {
				writeErr(w, 422, "Validation Failed: "+err.Error())
				return
			}
			if iss == nil {
				writeErr(w, 404, "Not Found")
				return
			}
			buf := store.MarshalIssueJSON(iss, make([]byte, 0, 512))
			_ = w.WriteJSON(200, append(buf, '\n'))
		default:
			writeErr(w, 404, "Not Found")
		}
		return
	}

	if sub == "/comments" {
		iss := repo.FastGetIssue(num)
		if iss == nil {
			writeErr(w, 404, "Not Found")
			return
		}
		switch r.Method {
		case "POST":
			var req struct {
				Body string `json:"body"`
			}
			if err := json.Unmarshal(r.Body, &req); err != nil {
				writeErr(w, 400, "Problems parsing JSON")
				return
			}
			c := st.AddComment(repo, num, req.Body)
			if c == nil {
				writeErr(w, 404, "Not Found")
				return
			}
			writeJSONVal(w, 201, commentJSON(c))
		case "GET":
			repo.DataMu.RLock()
			out := make([]map[string]any, 0, len(iss.Comments))
			for _, c := range iss.Comments {
				out = append(out, commentJSON(c))
			}
			repo.DataMu.RUnlock()
			writeJSONVal(w, 200, out)
		default:
			writeErr(w, 404, "Not Found")
		}
		return
	}

	writeErr(w, 404, "Not Found")
}

func commentJSON(c *store.Comment) map[string]any {
	return map[string]any{
		"id":         c.ID,
		"node_id":    c.GlobalID,
		"body":       c.Body,
		"user":       map[string]any{"id": store.ViewerID, "login": c.AuthorLogin},
		"created_at": c.CreatedAt,
		"updated_at": c.UpdatedAt,
	}
}

func pullJSON(pr *store.PullRequest) map[string]any {
	var mergedAt any = nil
	if pr.MergedAt != "" {
		mergedAt = pr.MergedAt
	}
	var mergeSHA any = nil
	if pr.MergeCommitSHA != "" {
		mergeSHA = pr.MergeCommitSHA
	}
	return map[string]any{
		"id":               pr.ID,
		"node_id":          pr.GlobalID,
		"number":           pr.Number,
		"state":            pr.State,
		"title":            pr.Title,
		"body":             pr.Body,
		"merged":           pr.Merged,
		"merged_at":        mergedAt,
		"merge_commit_sha": mergeSHA,
		"head": map[string]any{
			"ref": pr.HeadRef,
			"sha": pr.HeadSHA,
		},
		"base": map[string]any{
			"ref": pr.BaseRef,
			"sha": pr.BaseSHA,
		},
		"user":       map[string]any{"id": store.ViewerID, "login": pr.AuthorLogin},
		"created_at": pr.CreatedAt,
		"updated_at": pr.UpdatedAt,
	}
}

func handleCreatePull(w *http1.ResponseWriter, r *http1.Request, st *store.Store, repo *store.Repo) {
	var req struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Head  string `json:"head"`
		Base  string `json:"base"`
	}
	if err := json.Unmarshal(r.Body, &req); err != nil {
		writeErr(w, 400, "Problems parsing JSON")
		return
	}
	headRef := req.Head
	if colon := strings.IndexByte(headRef, ':'); colon >= 0 {
		headRef = headRef[colon+1:]
	}
	baseRef := req.Base
	if baseRef == "" {
		baseRef = repo.DefaultBranch
	}

	repo.GitMu.RLock()
	headOID, ok1 := repo.Refs["refs/heads/"+headRef]
	baseOID, ok2 := repo.Refs["refs/heads/"+baseRef]
	repo.GitMu.RUnlock()
	if !ok1 || !ok2 {
		writeErr(w, 422, "Validation Failed: head or base branch not found")
		return
	}

	pr := st.CreatePullRequest(repo, req.Title, req.Body, headRef, git.FormatOID(headOID), baseRef, git.FormatOID(baseOID))
	writeJSONVal(w, 201, pullJSON(pr))
}

func handleListPulls(w *http1.ResponseWriter, r *http1.Request, st *store.Store, repo *store.Repo) {
	state := r.QueryParam("state")
	if state == "" {
		state = "open"
	}
	list := st.ListPullRequests(repo, state)
	out := make([]map[string]any, 0, len(list))
	for _, pr := range list {
		out = append(out, pullJSON(pr))
	}
	writeJSONVal(w, 200, out)
}

func handlePullsSub(w *http1.ResponseWriter, r *http1.Request, st *store.Store, repo *store.Repo, rest string) {
	var numStr, sub string
	if s := strings.IndexByte(rest, '/'); s < 0 {
		numStr = rest
	} else {
		numStr = rest[:s]
		sub = rest[s:]
	}
	num, ok := parseFastInt64(numStr)
	if !ok || num <= 0 {
		writeErr(w, 404, "Not Found")
		return
	}
	pr := st.GetPullRequest(repo, num)
	if pr == nil {
		writeErr(w, 404, "Not Found")
		return
	}

	switch {
	case sub == "" && r.Method == "GET":
		repo.DataMu.RLock()
		res := pullJSON(pr)
		repo.DataMu.RUnlock()
		writeJSONVal(w, 200, res)

	case sub == "/files" && r.Method == "GET":
		headOID, ok1 := git.ParseOID(pr.HeadSHA)
		baseOID, ok2 := git.ParseOID(pr.BaseSHA)
		if !ok1 || !ok2 {
			writeErr(w, 500, "Invalid PR shas")
			return
		}
		repo.GitMu.RLock()
		if curHead, ok := repo.Refs["refs/heads/"+pr.HeadRef]; ok {
			headOID = curHead
		}
		if curBase, ok := repo.Refs["refs/heads/"+pr.BaseRef]; ok {
			if lca, hasLCA := git.MergeBaseLocked(repo, curBase, headOID); hasLCA {
				baseOID = lca
			}
		}
		diffs, err := git.DiffCommitsLocked(repo, baseOID, headOID)
		repo.GitMu.RUnlock()
		if err != nil {
			writeErr(w, 500, "Failed to compute diff")
			return
		}
		out := make([]map[string]any, 0, len(diffs))
		for _, d := range diffs {
			out = append(out, map[string]any{
				"filename":  d.Filename,
				"status":    d.Status,
				"additions": d.Additions,
				"deletions": d.Deletions,
				"changes":   d.Changes,
			})
		}
		writeJSONVal(w, 200, out)

	case sub == "/merge" && r.Method == "PUT":
		var req struct {
			MergeMethod   string `json:"merge_method"`
			CommitTitle   string `json:"commit_title"`
			CommitMessage string `json:"commit_message"`
		}
		if len(r.Body) > 0 {
			if err := json.Unmarshal(r.Body, &req); err != nil {
				writeErr(w, 400, "Problems parsing JSON")
				return
			}
		}
		if req.MergeMethod == "" {
			req.MergeMethod = "merge"
		}
		shaHex, merged, errMsg, status := MergePullRequest(repo, pr, req.MergeMethod, req.CommitTitle)
		if !merged {
			writeErr(w, status, errMsg)
			return
		}
		writeJSONVal(w, 200, map[string]any{
			"sha":     shaHex,
			"merged":  true,
			"message": "Pull Request successfully merged",
		})

	default:
		writeErr(w, 404, "Not Found")
	}
}

// MergePullRequest merges pr into its base branch and updates git + PR state atomically.
// Shared across REST, GraphQL, and MCP.
func MergePullRequest(repo *store.Repo, pr *store.PullRequest, method, commitMsg string) (shaHex string, ok bool, errMsg string, status int) {
	repo.DataMu.Lock()
	if pr.Merged || pr.State == "closed" {
		repo.DataMu.Unlock()
		return "", false, "Pull Request is not mergeable", 405
	}
	baseRef := pr.BaseRef
	headRef := pr.HeadRef
	headSHAStr := pr.HeadSHA
	prNum := pr.Number
	prTitle := pr.Title
	repo.DataMu.Unlock()

	repo.GitMu.Lock()
	baseOID, okBase := repo.Refs["refs/heads/"+baseRef]
	headOID, okHead := repo.Refs["refs/heads/"+headRef]
	if !okHead {
		if parsed, okP := git.ParseOID(headSHAStr); okP {
			headOID = parsed
			okHead = true
		}
	}
	if !okBase || !okHead {
		repo.GitMu.Unlock()
		return "", false, "Pull Request branch not found", 405
	}

	mergedTree, conflict, err := git.MergeCommitsLocked(repo, baseOID, headOID)
	if err != nil {
		repo.GitMu.Unlock()
		return "", false, "Merge failed: " + err.Error(), 500
	}
	if conflict {
		repo.GitMu.Unlock()
		return "", false, "Pull Request is not mergeable", 405
	}

	parents := [][20]byte{baseOID}
	if strings.EqualFold(method, "merge") {
		parents = append(parents, headOID)
	}
	msg := commitMsg
	if msg == "" {
		msg = "Merge pull request #" + strconv.FormatInt(prNum, 10) + ": " + prTitle
	}
	ts := int64(1735689600 + len(repo.Commits))
	commitBytes := git.BuildCommit(mergedTree, parents, store.ViewerName, store.ViewerEmail, ts, msg)
	mergeCommitOID, err := git.PutObjectLocked(repo, store.ObjCommit, commitBytes)
	if err != nil {
		repo.GitMu.Unlock()
		return "", false, "Failed to write merge commit", 500
	}
	repo.Refs["refs/heads/"+baseRef] = mergeCommitOID
	repo.InvalidateGitCaches()
	repo.GitMu.Unlock()

	shaHex = git.FormatOID(mergeCommitOID)
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	repo.DataMu.Lock()
	pr.Merged = true
	pr.State = "closed"
	pr.MergedAt = now
	pr.MergeCommitSHA = shaHex
	pr.UpdatedAt = now
	repo.DataMu.Unlock()

	return shaHex, true, "", 200
}
