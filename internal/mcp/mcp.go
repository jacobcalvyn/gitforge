package mcp

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"sync/atomic"

	"gitforge/internal/git"
	"gitforge/internal/http1"
	"gitforge/internal/rest"
	"gitforge/internal/store"
)

var sessionSeq atomic.Uint64

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// toolsListJSON is strictly static and deterministic so repeated tools/list calls are byte-identical.
var toolsListJSON = json.RawMessage(`{"tools":[` +
	`{"name":"create_repo","description":"Create a repository for the authenticated user","inputSchema":{"type":"object","properties":{"name":{"type":"string","description":"Repository name"},"owner":{"type":"string","description":"Owner login"},"description":{"type":"string","description":"Repository description"},"private":{"type":"boolean","description":"Whether the repository is private"}},"required":["name"]}},` +
	`{"name":"get_repo","description":"Get repository details by owner and name","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner"},"repo":{"type":"string","description":"Repository name"}},"required":["owner","repo"]}},` +
	`{"name":"create_issue","description":"Create a new issue in a repository","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner"},"repo":{"type":"string","description":"Repository name"},"title":{"type":"string","description":"Issue title"},"body":{"type":"string","description":"Issue body"}},"required":["owner","repo","title"]}},` +
	`{"name":"list_issues","description":"List issues in a repository","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner"},"repo":{"type":"string","description":"Repository name"},"state":{"type":"string","description":"Filter by state (open, closed, all)"}},"required":["owner","repo"]}},` +
	`{"name":"create_pull_request","description":"Create a new pull request","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner"},"repo":{"type":"string","description":"Repository name"},"title":{"type":"string","description":"Pull request title"},"body":{"type":"string","description":"Pull request body"},"head":{"type":"string","description":"Head branch name"},"base":{"type":"string","description":"Base branch name"}},"required":["owner","repo","title","head","base"]}},` +
	`{"name":"merge_pull_request","description":"Merge an open pull request","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner"},"repo":{"type":"string","description":"Repository name"},"pull_number":{"type":"integer","description":"Pull request number"},"merge_method":{"type":"string","description":"Merge method (merge, squash, rebase)"}},"required":["owner","repo","pull_number"]}}` +
	`]}`)

// HandleMCP serves Streamable HTTP MCP requests at /mcp.
func HandleMCP(w *http1.ResponseWriter, r *http1.Request, st *store.Store) {
	if r.Method == "DELETE" {
		_ = w.WriteResponse(204, "", nil, nil)
		return
	}
	if r.Method != "POST" {
		_ = w.WriteJSON(405, []byte(`{"message":"Method Not Allowed"}`+"\n"))
		return
	}

	body := bytes.TrimSpace(r.Body)
	if len(body) == 0 || body[0] != '{' {
		_ = w.WriteJSON(400, []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}`+"\n"))
		return
	}

	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		_ = w.WriteJSON(400, []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}`+"\n"))
		return
	}

	// Notifications carry no id.
	trimmedID := bytes.TrimSpace(req.ID)
	if len(trimmedID) == 0 || strings.HasPrefix(req.Method, "notifications/") {
		_ = w.WriteResponse(202, "", nil, nil)
		return
	}

	var extraHeaders []http1.Header
	if r.MCPProtocolVersion != "" {
		extraHeaders = append(extraHeaders, http1.Header{Name: "MCP-Protocol-Version", Value: r.MCPProtocolVersion})
	} else {
		extraHeaders = append(extraHeaders, http1.Header{Name: "MCP-Protocol-Version", Value: "2026-07-28"})
	}

	switch req.Method {
	case "server/discover":
		writeRPCResult(w, extraHeaders, req.ID, map[string]any{
			"supportedVersions": []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"},
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"serverInfo":        map[string]any{"name": "gitforge", "version": "1.0.0"},
			"_meta": map[string]any{
				"io.modelcontextprotocol/serverInfo": map[string]any{"name": "gitforge", "version": "1.0.0"},
			},
		})

	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if len(req.Params) > 0 {
			_ = json.Unmarshal(req.Params, &params)
		}
		proto := params.ProtocolVersion
		if proto == "" {
			proto = "2025-11-25"
		}
		sid := "mcp-sess-" + strconv.FormatUint(sessionSeq.Add(1), 10)
		extraHeaders = append(extraHeaders, http1.Header{Name: "Mcp-Session-Id", Value: sid})
		writeRPCResult(w, extraHeaders, req.ID, map[string]any{
			"protocolVersion": proto,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "gitforge", "version": "1.0.0"},
		})

	case "tools/list":
		writeRPCResult(w, extraHeaders, req.ID, toolsListJSON)

	case "tools/call":
		handleToolCall(w, extraHeaders, req, st)

	default:
		writeRPCError(w, extraHeaders, req.ID, -32601, "Method not found: "+req.Method)
	}
}

func writeRPCResult(w *http1.ResponseWriter, headers []http1.Header, id json.RawMessage, result any) {
	b, _ := json.Marshal(rpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	})
	_ = w.WriteJSONWithHeaders(200, headers, append(b, '\n'))
}

func writeRPCError(w *http1.ResponseWriter, headers []http1.Header, id json.RawMessage, code int, msg string) {
	b, _ := json.Marshal(rpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &rpcError{Code: code, Message: msg},
	})
	_ = w.WriteJSONWithHeaders(200, headers, append(b, '\n'))
}

func toolSuccess(w *http1.ResponseWriter, headers []http1.Header, id json.RawMessage, payload any) {
	txt, _ := json.Marshal(payload)
	writeRPCResult(w, headers, id, map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": string(txt)},
		},
		"structuredContent": payload,
		"isError":           false,
	})
}

func toolErr(w *http1.ResponseWriter, headers []http1.Header, id json.RawMessage, msg string) {
	writeRPCResult(w, headers, id, map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": msg},
		},
		"isError": true,
	})
}

func handleToolCall(w *http1.ResponseWriter, headers []http1.Header, req rpcRequest, st *store.Store) {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeRPCError(w, headers, req.ID, -32602, "Invalid params")
		return
	}
	args := params.Arguments
	if args == nil {
		args = map[string]any{}
	}

	strArg := func(key string) string {
		s, _ := args[key].(string)
		return s
	}
	boolArg := func(key string) bool {
		b, _ := args[key].(bool)
		return b
	}
	intArg := func(key string) (int64, bool) {
		switch v := args[key].(type) {
		case float64:
			return int64(v), true
		case int64:
			return v, true
		case string:
			n, err := strconv.ParseInt(v, 10, 64)
			return n, err == nil
		default:
			return 0, false
		}
	}

	switch params.Name {
	case "create_repo":
		name := strings.TrimSpace(strArg("name"))
		if name == "" {
			writeRPCError(w, headers, req.ID, -32602, "Missing required argument: name")
			return
		}
		owner := strArg("owner")
		if owner == "" {
			owner = store.ViewerLogin
		}
		repo, created := st.CreateRepo(owner, name, strArg("description"), boolArg("private"), "main")
		if !created {
			toolErr(w, headers, req.ID, "Repository already exists")
			return
		}
		toolSuccess(w, headers, req.ID, map[string]any{
			"id":             repo.ID,
			"node_id":        repo.GlobalID,
			"name":           repo.Name,
			"full_name":      repo.FullName,
			"private":        repo.Private,
			"description":    repo.Description,
			"default_branch": repo.DefaultBranch,
			"owner":          map[string]any{"id": store.ViewerID, "login": repo.Owner},
		})

	case "get_repo":
		owner := strArg("owner")
		repoName := strArg("repo")
		if owner == "" || repoName == "" {
			writeRPCError(w, headers, req.ID, -32602, "Missing required arguments: owner, repo")
			return
		}
		repo := st.GetRepo(owner, repoName)
		if repo == nil {
			toolErr(w, headers, req.ID, "Repository not found")
			return
		}
		repo.DataMu.RLock()
		res := map[string]any{
			"id":             repo.ID,
			"node_id":        repo.GlobalID,
			"name":           repo.Name,
			"full_name":      repo.FullName,
			"private":        repo.Private,
			"description":    repo.Description,
			"default_branch": repo.DefaultBranch,
			"owner":          map[string]any{"id": store.ViewerID, "login": repo.Owner},
		}
		repo.DataMu.RUnlock()
		toolSuccess(w, headers, req.ID, res)

	case "create_issue":
		owner := strArg("owner")
		repoName := strArg("repo")
		title := strArg("title")
		if owner == "" || repoName == "" || title == "" {
			writeRPCError(w, headers, req.ID, -32602, "Missing required arguments: owner, repo, title")
			return
		}
		repo := st.GetRepo(owner, repoName)
		if repo == nil {
			toolErr(w, headers, req.ID, "Repository not found")
			return
		}
		iss, err := st.CreateIssue(repo, title, strArg("body"), nil)
		if err != nil {
			toolErr(w, headers, req.ID, err.Error())
			return
		}
		toolSuccess(w, headers, req.ID, map[string]any{
			"id":      iss.ID,
			"node_id": iss.GlobalID,
			"number":  iss.Number,
			"title":   iss.Title,
			"body":    iss.Body,
			"state":   iss.State,
		})

	case "list_issues":
		owner := strArg("owner")
		repoName := strArg("repo")
		if owner == "" || repoName == "" {
			writeRPCError(w, headers, req.ID, -32602, "Missing required arguments: owner, repo")
			return
		}
		repo := st.GetRepo(owner, repoName)
		if repo == nil {
			toolErr(w, headers, req.ID, "Repository not found")
			return
		}
		state := strArg("state")
		if state == "" {
			state = "open"
		}
		issues := st.ListIssues(repo, state, "desc")
		out := make([]map[string]any, 0, len(issues))
		for _, iss := range issues {
			out = append(out, map[string]any{
				"id":      iss.ID,
				"node_id": iss.GlobalID,
				"number":  iss.Number,
				"title":   iss.Title,
				"body":    iss.Body,
				"state":   iss.State,
			})
		}
		toolSuccess(w, headers, req.ID, map[string]any{"issues": out})

	case "create_pull_request":
		owner := strArg("owner")
		repoName := strArg("repo")
		title := strArg("title")
		head := strArg("head")
		base := strArg("base")
		if owner == "" || repoName == "" || title == "" || head == "" || base == "" {
			writeRPCError(w, headers, req.ID, -32602, "Missing required arguments: owner, repo, title, head, base")
			return
		}
		repo := st.GetRepo(owner, repoName)
		if repo == nil {
			toolErr(w, headers, req.ID, "Repository not found")
			return
		}
		repo.GitMu.RLock()
		headOID, ok1 := repo.Refs["refs/heads/"+head]
		baseOID, ok2 := repo.Refs["refs/heads/"+base]
		repo.GitMu.RUnlock()
		if !ok1 || !ok2 {
			toolErr(w, headers, req.ID, "Branch not found")
			return
		}
		pr := st.CreatePullRequest(repo, title, strArg("body"), head, git.FormatOID(headOID), base, git.FormatOID(baseOID))
		toolSuccess(w, headers, req.ID, map[string]any{
			"id":      pr.ID,
			"node_id": pr.GlobalID,
			"number":  pr.Number,
			"state":   pr.State,
			"title":   pr.Title,
			"body":    pr.Body,
			"head":    map[string]any{"ref": pr.HeadRef, "sha": pr.HeadSHA},
			"base":    map[string]any{"ref": pr.BaseRef, "sha": pr.BaseSHA},
		})

	case "merge_pull_request":
		owner := strArg("owner")
		repoName := strArg("repo")
		pullNum, ok := intArg("pull_number")
		if owner == "" || repoName == "" || !ok || pullNum <= 0 {
			writeRPCError(w, headers, req.ID, -32602, "Missing required arguments: owner, repo, pull_number")
			return
		}
		repo := st.GetRepo(owner, repoName)
		if repo == nil {
			toolErr(w, headers, req.ID, "Repository not found")
			return
		}
		pr := st.GetPullRequest(repo, pullNum)
		if pr == nil {
			toolErr(w, headers, req.ID, "Pull request not found")
			return
		}
		method := strArg("merge_method")
		if method == "" {
			method = "merge"
		}
		shaHex, merged, errMsg, _ := rest.MergePullRequest(repo, pr, method, "")
		if !merged {
			toolErr(w, headers, req.ID, errMsg)
			return
		}
		toolSuccess(w, headers, req.ID, map[string]any{
			"sha":     shaHex,
			"merged":  true,
			"message": "Pull Request successfully merged",
		})

	default:
		writeRPCError(w, headers, req.ID, -32601, "Unknown tool: "+params.Name)
	}
}
