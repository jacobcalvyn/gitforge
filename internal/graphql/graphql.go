package graphql

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"gitforge/internal/git"
	"gitforge/internal/http1"
	"gitforge/internal/rest"
	"gitforge/internal/store"
)

type requestEnvelope struct {
	Query         string         `json:"query"`
	Variables     map[string]any `json:"variables"`
	OperationName string         `json:"operationName"`
}

type gqlError struct {
	Message string `json:"message"`
}

type responseEnvelope struct {
	Data   any        `json:"data"`
	Errors []gqlError `json:"errors,omitempty"`
}

// HandleGraphQL serves POST /api/graphql (single operations and batched arrays).
func HandleGraphQL(w *http1.ResponseWriter, r *http1.Request, st *store.Store) {
	if r.Method != "POST" {
		b := []byte(`{"message":"Method Not Allowed"}` + "\n")
		_ = w.WriteJSON(405, b)
		return
	}

	body := bytes.TrimSpace(r.Body)
	if len(body) == 0 {
		b := []byte(`{"message":"Empty request body"}` + "\n")
		_ = w.WriteJSON(400, b)
		return
	}

	if body[0] == '[' {
		var ops []requestEnvelope
		if err := json.Unmarshal(body, &ops); err != nil {
			_ = w.WriteJSON(400, []byte(`{"message":"Problems parsing JSON"}`+"\n"))
			return
		}
		results := make([]responseEnvelope, len(ops))
		for i, op := range ops {
			results[i] = executeOperation(op, st)
		}
		out, _ := json.Marshal(results)
		_ = w.WriteJSON(200, append(out, '\n'))
		return
	}

	var op requestEnvelope
	if err := json.Unmarshal(body, &op); err != nil {
		_ = w.WriteJSON(400, []byte(`{"message":"Problems parsing JSON"}`+"\n"))
		return
	}
	res := executeOperation(op, st)
	out, _ := json.Marshal(res)
	_ = w.WriteJSON(200, append(out, '\n'))
}

// -----------------------------------------------------------------------------
// Lexer & AST Parser
// -----------------------------------------------------------------------------

type tokKind int

const (
	tokEOF tokKind = iota
	tokName
	tokInt
	tokFloat
	tokString
	tokPunct
)

type token struct {
	kind tokKind
	val  string
}

func tokenize(src string) ([]token, error) {
	var toks []token
	i := 0
	n := len(src)
	for i < n {
		c := src[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			i++
			continue
		}
		if c == '#' {
			for i < n && src[i] != '\n' {
				i++
			}
			continue
		}
		if c == '.' && i+2 < n && src[i+1] == '.' && src[i+2] == '.' {
			toks = append(toks, token{kind: tokPunct, val: "..."})
			i += 3
			continue
		}
		if strings.ContainsRune("{}()[]:=@!$", rune(c)) {
			toks = append(toks, token{kind: tokPunct, val: string(c)})
			i++
			continue
		}
		if c == '"' {
			if i+2 < n && src[i+1] == '"' && src[i+2] == '"' {
				// Block string
				i += 3
				start := i
				for i+2 < n && !(src[i] == '"' && src[i+1] == '"' && src[i+2] == '"') {
					i++
				}
				if i+2 >= n {
					return nil, fmt.Errorf("unterminated block string")
				}
				toks = append(toks, token{kind: tokString, val: src[start:i]})
				i += 3
				continue
			}
			i++
			var sb strings.Builder
			closed := false
			for i < n {
				ch := src[i]
				if ch == '"' {
					i++
					closed = true
					break
				}
				if ch == '\\' && i+1 < n {
					i++
					esc := src[i]
					switch esc {
					case '"', '\\', '/':
						sb.WriteByte(esc)
					case 'b':
						sb.WriteByte('\b')
					case 'f':
						sb.WriteByte('\f')
					case 'n':
						sb.WriteByte('\n')
					case 'r':
						sb.WriteByte('\r')
					case 't':
						sb.WriteByte('\t')
					default:
						sb.WriteByte(esc)
					}
					i++
					continue
				}
				sb.WriteByte(ch)
				i++
			}
			if !closed {
				return nil, fmt.Errorf("unterminated string")
			}
			toks = append(toks, token{kind: tokString, val: sb.String()})
			continue
		}
		if c == '-' || (c >= '0' && c <= '9') {
			start := i
			if c == '-' {
				i++
			}
			isFloat := false
			for i < n && src[i] >= '0' && src[i] <= '9' {
				i++
			}
			if i < n && src[i] == '.' {
				isFloat = true
				i++
				for i < n && src[i] >= '0' && src[i] <= '9' {
					i++
				}
			}
			if i < n && (src[i] == 'e' || src[i] == 'E') {
				isFloat = true
				i++
				if i < n && (src[i] == '+' || src[i] == '-') {
					i++
				}
				for i < n && src[i] >= '0' && src[i] <= '9' {
					i++
				}
			}
			k := tokInt
			if isFloat {
				k = tokFloat
			}
			toks = append(toks, token{kind: k, val: src[start:i]})
			continue
		}
		if isNameStart(c) {
			start := i
			i++
			for i < n && isNameContinue(src[i]) {
				i++
			}
			toks = append(toks, token{kind: tokName, val: src[start:i]})
			continue
		}
		return nil, fmt.Errorf("unexpected character %q", c)
	}
	toks = append(toks, token{kind: tokEOF})
	return toks, nil
}

func isNameStart(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_'
}

func isNameContinue(c byte) bool {
	return isNameStart(c) || (c >= '0' && c <= '9')
}

type document struct {
	ops       []*operationDef
	fragments map[string]*fragmentDef
}

type varDef struct {
	name       string
	hasDefault bool
	defVal     valueNode
}

type operationDef struct {
	opType string // "query" or "mutation"
	name   string
	vars   []varDef
	sels   []selection
}

type fragmentDef struct {
	name     string
	typeCond string
	sels     []selection
}

type selection struct {
	isInline bool
	isSpread bool
	alias    string
	name     string
	typeCond string
	args     map[string]valueNode
	sels     []selection
}

type valueKind int

const (
	valNull valueKind = iota
	valVar
	valInt
	valFloat
	valString
	valBool
	valEnum
	valList
	valObject
)

type valueNode struct {
	kind   valueKind
	str    string
	i64    int64
	f64    float64
	b      bool
	list   []valueNode
	fields map[string]valueNode
}

type parser struct {
	toks []token
	pos  int
}

func (p *parser) peek() token {
	if p.pos >= len(p.toks) {
		return token{kind: tokEOF}
	}
	return p.toks[p.pos]
}

func (p *parser) next() token {
	t := p.peek()
	if p.pos < len(p.toks) {
		p.pos++
	}
	return t
}

func (p *parser) expectPunct(v string) error {
	t := p.next()
	if t.kind != tokPunct || t.val != v {
		return fmt.Errorf("expected %q, got %q", v, t.val)
	}
	return nil
}

func parseDocument(src string) (*document, error) {
	toks, err := tokenize(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	doc := &document{fragments: make(map[string]*fragmentDef)}
	for p.peek().kind != tokEOF {
		t := p.peek()
		if t.kind == tokPunct && t.val == "{" {
			sels, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			doc.ops = append(doc.ops, &operationDef{opType: "query", sels: sels})
			continue
		}
		if t.kind == tokName && (t.val == "query" || t.val == "mutation" || t.val == "subscription") {
			opType := p.next().val
			op := &operationDef{opType: opType}
			if p.peek().kind == tokName {
				op.name = p.next().val
			}
			if p.peek().kind == tokPunct && p.peek().val == "(" {
				vars, err := p.parseVarDefs()
				if err != nil {
					return nil, err
				}
				op.vars = vars
			}
			if err := p.skipDirectives(); err != nil {
				return nil, err
			}
			sels, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			op.sels = sels
			doc.ops = append(doc.ops, op)
			continue
		}
		if t.kind == tokName && t.val == "fragment" {
			p.next()
			nameTok := p.next()
			if nameTok.kind != tokName {
				return nil, fmt.Errorf("expected fragment name")
			}
			onTok := p.next()
			if onTok.kind != tokName || onTok.val != "on" {
				return nil, fmt.Errorf("expected 'on'")
			}
			typeTok := p.next()
			if typeTok.kind != tokName {
				return nil, fmt.Errorf("expected type condition")
			}
			if err := p.skipDirectives(); err != nil {
				return nil, err
			}
			sels, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			doc.fragments[nameTok.val] = &fragmentDef{name: nameTok.val, typeCond: typeTok.val, sels: sels}
			continue
		}
		return nil, fmt.Errorf("unexpected token %q", t.val)
	}
	return doc, nil
}

func (p *parser) parseVarDefs() ([]varDef, error) {
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	var vars []varDef
	for !(p.peek().kind == tokPunct && p.peek().val == ")") {
		if err := p.expectPunct("$"); err != nil {
			return nil, err
		}
		nameTok := p.next()
		if nameTok.kind != tokName {
			return nil, fmt.Errorf("expected variable name")
		}
		if err := p.expectPunct(":"); err != nil {
			return nil, err
		}
		if err := p.skipTypeRef(); err != nil {
			return nil, err
		}
		vd := varDef{name: nameTok.val}
		if p.peek().kind == tokPunct && p.peek().val == "=" {
			p.next()
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			vd.hasDefault = true
			vd.defVal = v
		}
		_ = p.skipDirectives()
		vars = append(vars, vd)
	}
	p.next()
	return vars, nil
}

func (p *parser) skipTypeRef() error {
	if p.peek().kind == tokPunct && p.peek().val == "[" {
		p.next()
		if err := p.skipTypeRef(); err != nil {
			return err
		}
		if err := p.expectPunct("]"); err != nil {
			return err
		}
	} else {
		if p.next().kind != tokName {
			return fmt.Errorf("expected type name")
		}
	}
	if p.peek().kind == tokPunct && p.peek().val == "!" {
		p.next()
	}
	return nil
}

func (p *parser) skipDirectives() error {
	for p.peek().kind == tokPunct && p.peek().val == "@" {
		p.next()
		if p.next().kind != tokName {
			return fmt.Errorf("expected directive name")
		}
		if p.peek().kind == tokPunct && p.peek().val == "(" {
			if _, err := p.parseArguments(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *parser) parseSelectionSet() ([]selection, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var sels []selection
	for !(p.peek().kind == tokPunct && p.peek().val == "}") {
		if p.peek().kind == tokEOF {
			return nil, fmt.Errorf("unexpected EOF in selection set")
		}
		if p.peek().kind == tokPunct && p.peek().val == "..." {
			p.next()
			if p.peek().kind == tokName && p.peek().val == "on" {
				p.next()
				typeTok := p.next()
				if typeTok.kind != tokName {
					return nil, fmt.Errorf("expected type name after ... on")
				}
				if err := p.skipDirectives(); err != nil {
					return nil, err
				}
				sub, err := p.parseSelectionSet()
				if err != nil {
					return nil, err
				}
				sels = append(sels, selection{isInline: true, typeCond: typeTok.val, sels: sub})
			} else if p.peek().kind == tokPunct && p.peek().val == "{" {
				sub, err := p.parseSelectionSet()
				if err != nil {
					return nil, err
				}
				sels = append(sels, selection{isInline: true, sels: sub})
			} else {
				fragTok := p.next()
				if fragTok.kind != tokName {
					return nil, fmt.Errorf("expected fragment name")
				}
				if err := p.skipDirectives(); err != nil {
					return nil, err
				}
				sels = append(sels, selection{isSpread: true, name: fragTok.val})
			}
			continue
		}

		first := p.next()
		if first.kind != tokName {
			return nil, fmt.Errorf("expected field name, got %q", first.val)
		}
		sel := selection{name: first.val}
		if p.peek().kind == tokPunct && p.peek().val == ":" {
			p.next()
			fieldTok := p.next()
			if fieldTok.kind != tokName {
				return nil, fmt.Errorf("expected field name after alias")
			}
			sel.alias = first.val
			sel.name = fieldTok.val
		}
		if p.peek().kind == tokPunct && p.peek().val == "(" {
			args, err := p.parseArguments()
			if err != nil {
				return nil, err
			}
			sel.args = args
		}
		if err := p.skipDirectives(); err != nil {
			return nil, err
		}
		if p.peek().kind == tokPunct && p.peek().val == "{" {
			sub, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			sel.sels = sub
		}
		sels = append(sels, sel)
	}
	p.next()
	return sels, nil
}

func (p *parser) parseArguments() (map[string]valueNode, error) {
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	args := make(map[string]valueNode)
	for !(p.peek().kind == tokPunct && p.peek().val == ")") {
		nameTok := p.next()
		if nameTok.kind != tokName {
			return nil, fmt.Errorf("expected argument name")
		}
		if err := p.expectPunct(":"); err != nil {
			return nil, err
		}
		val, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		args[nameTok.val] = val
	}
	p.next()
	return args, nil
}

func (p *parser) parseValue() (valueNode, error) {
	t := p.next()
	switch t.kind {
	case tokInt:
		v, err := strconv.ParseInt(t.val, 10, 64)
		if err != nil {
			return valueNode{}, err
		}
		return valueNode{kind: valInt, i64: v}, nil
	case tokFloat:
		v, err := strconv.ParseFloat(t.val, 64)
		if err != nil {
			return valueNode{}, err
		}
		return valueNode{kind: valFloat, f64: v}, nil
	case tokString:
		return valueNode{kind: valString, str: t.val}, nil
	case tokName:
		switch t.val {
		case "true":
			return valueNode{kind: valBool, b: true}, nil
		case "false":
			return valueNode{kind: valBool, b: false}, nil
		case "null":
			return valueNode{kind: valNull}, nil
		default:
			return valueNode{kind: valEnum, str: t.val}, nil
		}
	case tokPunct:
		switch t.val {
		case "$":
			nt := p.next()
			if nt.kind != tokName {
				return valueNode{}, fmt.Errorf("expected variable name after $")
			}
			return valueNode{kind: valVar, str: nt.val}, nil
		case "[":
			var list []valueNode
			for !(p.peek().kind == tokPunct && p.peek().val == "]") {
				item, err := p.parseValue()
				if err != nil {
					return valueNode{}, err
				}
				list = append(list, item)
			}
			p.next()
			return valueNode{kind: valList, list: list}, nil
		case "{":
			fields := make(map[string]valueNode)
			for !(p.peek().kind == tokPunct && p.peek().val == "}") {
				kt := p.next()
				if kt.kind != tokName {
					return valueNode{}, fmt.Errorf("expected object field name")
				}
				if err := p.expectPunct(":"); err != nil {
					return valueNode{}, err
				}
				v, err := p.parseValue()
				if err != nil {
					return valueNode{}, err
				}
				fields[kt.val] = v
			}
			p.next()
			return valueNode{kind: valObject, fields: fields}, nil
		}
	}
	return valueNode{}, fmt.Errorf("unexpected value token %q", t.val)
}

// -----------------------------------------------------------------------------
// Schema Metadata & Execution Engine
// -----------------------------------------------------------------------------

var schemaFields = map[string][]string{
	"Query":                        {"viewer", "repository", "node", "__type"},
	"Mutation":                     {"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"},
	"User":                         {"id", "databaseId", "login", "name", "email"},
	"Ref":                          {"id", "name", "prefix", "target"},
	"GitObject":                    {"oid", "abbreviatedOid"},
	"Commit":                       {"id", "oid", "abbreviatedOid", "message"},
	"Repository":                   {"id", "databaseId", "name", "nameWithOwner", "owner", "description", "isPrivate", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequest", "pullRequests"},
	"Issue":                        {"id", "databaseId", "number", "title", "state", "body", "author", "createdAt", "updatedAt", "closedAt", "comments", "labels"},
	"IssueConnection":              {"nodes", "edges", "pageInfo", "totalCount"},
	"IssueEdge":                    {"cursor", "node"},
	"PullRequest":                  {"id", "databaseId", "number", "title", "body", "headRefName", "baseRefName", "mergeable", "state", "merged", "mergedAt", "mergeCommit", "files", "author", "createdAt", "updatedAt"},
	"PullRequestConnection":        {"nodes", "edges", "pageInfo", "totalCount"},
	"PullRequestEdge":              {"cursor", "node"},
	"PullRequestChangedFile":       {"path", "additions", "deletions", "changeType"},
	"PullRequestChangedFileConnection": {"nodes", "edges", "pageInfo", "totalCount"},
	"IssueComment":                 {"id", "databaseId", "body", "author", "createdAt", "updatedAt"},
	"IssueCommentEdge":             {"cursor", "node"},
	"IssueCommentConnection":       {"nodes", "edges", "pageInfo", "totalCount"},
	"PageInfo":                     {"hasNextPage", "hasPreviousPage", "startCursor", "endCursor"},
	"CreateIssuePayload":           {"issue", "clientMutationId"},
	"CloseIssuePayload":            {"issue", "clientMutationId"},
	"CreatePullRequestPayload":     {"pullRequest", "clientMutationId"},
	"MergePullRequestPayload":      {"pullRequest", "clientMutationId"},
	"AddCommentPayload":            {"commentEdge", "clientMutationId"},
	"__Type":                       {"kind", "name", "fields", "enumValues"},
	"__Field":                      {"name", "isDeprecated"},
	"__EnumValue":                  {"name", "isDeprecated"},
}

var schemaEnums = map[string][]string{
	"SearchType":             {"ISSUE", "REPOSITORY", "USER", "DISCUSSION"},
	"IssueState":             {"OPEN", "CLOSED"},
	"PullRequestState":       {"OPEN", "CLOSED", "MERGED"},
	"MergeableState":         {"MERGEABLE", "CONFLICTING", "UNKNOWN"},
	"PullRequestMergeMethod": {"MERGE", "SQUASH", "REBASE"},
	"RepositoryPermission":   {"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"},
	"IssueOrderField":        {"CREATED_AT", "UPDATED_AT", "COMMENTS"},
	"OrderDirection":         {"ASC", "DESC"},
}

type execCtx struct {
	st        *store.Store
	doc       *document
	vars      map[string]any
	errors    []gqlError
	abortData bool
}

func (e *execCtx) evalVal(n valueNode) any {
	switch n.kind {
	case valNull:
		return nil
	case valInt:
		return n.i64
	case valFloat:
		return n.f64
	case valString, valEnum:
		return n.str
	case valBool:
		return n.b
	case valVar:
		return e.vars[n.str]
	case valList:
		out := make([]any, len(n.list))
		for i, item := range n.list {
			out[i] = e.evalVal(item)
		}
		return out
	case valObject:
		out := make(map[string]any, len(n.fields))
		for k, v := range n.fields {
			out[k] = e.evalVal(v)
		}
		return out
	default:
		return nil
	}
}

func (e *execCtx) evalArgs(args map[string]valueNode) map[string]any {
	if len(args) == 0 {
		return nil
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		out[k] = e.evalVal(v)
	}
	return out
}

func (e *execCtx) flattenSelections(typeName string, sels []selection) []selection {
	var out []selection
	for _, s := range sels {
		if s.isInline {
			if s.typeCond == "" || s.typeCond == typeName || isInterfaceMatch(s.typeCond, typeName) {
				out = append(out, e.flattenSelections(typeName, s.sels)...)
			}
			continue
		}
		if s.isSpread {
			if frag, ok := e.doc.fragments[s.name]; ok {
				if frag.typeCond == "" || frag.typeCond == typeName || isInterfaceMatch(frag.typeCond, typeName) {
					out = append(out, e.flattenSelections(typeName, frag.sels)...)
				}
			}
			continue
		}
		out = append(out, s)
	}
	return out
}

func isInterfaceMatch(iface, concrete string) bool {
	if iface == "Node" {
		return concrete == "Repository" || concrete == "Issue" || concrete == "PullRequest" || concrete == "IssueComment" || concrete == "User"
	}
	if iface == "GitObject" {
		return concrete == "Commit"
	}
	return false
}

func (e *execCtx) checkField(typeName, fieldName string) bool {
	if fieldName == "__typename" {
		return true
	}
	allowed := schemaFields[typeName]
	for _, f := range allowed {
		if f == fieldName {
			return true
		}
	}
	e.errors = append(e.errors, gqlError{
		Message: fmt.Sprintf("Field '%s' doesn't exist on type '%s'", fieldName, typeName),
	})
	return false
}

func executeOperation(req requestEnvelope, st *store.Store) responseEnvelope {
	doc, err := parseDocument(req.Query)
	if err != nil {
		return responseEnvelope{Data: nil, Errors: []gqlError{{Message: err.Error()}}}
	}
	if len(doc.ops) == 0 {
		return responseEnvelope{Data: nil, Errors: []gqlError{{Message: "No operation found"}}}
	}
	var op *operationDef
	if req.OperationName != "" {
		for _, candidate := range doc.ops {
			if candidate.name == req.OperationName {
				op = candidate
				break
			}
		}
		if op == nil {
			return responseEnvelope{Data: nil, Errors: []gqlError{{Message: "Unknown operation " + req.OperationName}}}
		}
	} else {
		op = doc.ops[0]
	}

	vars := make(map[string]any)
	for k, v := range req.Variables {
		vars[k] = v
	}
	ctx := &execCtx{st: st, doc: doc, vars: vars}
	for _, vd := range op.vars {
		if _, exists := vars[vd.name]; !exists && vd.hasDefault {
			vars[vd.name] = ctx.evalVal(vd.defVal)
		}
	}

	var data any
	if op.opType == "mutation" {
		data = ctx.execObject("Mutation", nil, op.sels)
	} else {
		data = ctx.execObject("Query", nil, op.sels)
	}
	if ctx.abortData {
		data = nil
	}
	return responseEnvelope{Data: data, Errors: ctx.errors}
}

func keyOf(s selection) string {
	if s.alias != "" {
		return s.alias
	}
	return s.name
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asInt(v any, def int) int {
	switch x := v.(type) {
	case int64:
		return int(x)
	case int:
		return x
	case float64:
		return int(x)
	default:
		return def
	}
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asStringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		if ss, ok2 := v.([]string); ok2 {
			return ss
		}
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func (e *execCtx) execObject(typeName string, val any, sels []selection) map[string]any {
	flat := e.flattenSelections(typeName, sels)
	out := make(map[string]any, len(flat))
	for _, s := range flat {
		if !e.checkField(typeName, s.name) {
			continue
		}
		key := keyOf(s)
		if s.name == "__typename" {
			out[key] = typeName
			continue
		}
		args := e.evalArgs(s.args)
		out[key] = e.resolveField(typeName, val, s.name, args, s.sels)
	}
	return out
}

func (e *execCtx) resolveField(typeName string, obj any, field string, args map[string]any, sels []selection) any {
	switch typeName {
	case "Query":
		switch field {
		case "viewer":
			return e.execObject("User", map[string]any{
				"id":         store.EncodeGlobalID("User", store.ViewerID),
				"databaseId": store.ViewerID,
				"login":      store.ViewerLogin,
				"name":       store.ViewerName,
				"email":      store.ViewerEmail,
			}, sels)
		case "repository":
			owner := asString(args["owner"])
			name := asString(args["name"])
			repo := e.st.GetRepo(owner, name)
			if repo == nil {
				return nil
			}
			return e.execObject("Repository", repo, sels)
		case "node":
			id := asString(args["id"])
			if repo := e.st.GetRepoByGlobalID(id); repo != nil {
				return e.execObject("Repository", repo, sels)
			}
			if _, iss := e.st.GetIssueByGlobalID(id); iss != nil {
				return e.execObject("Issue", iss, sels)
			}
			if _, pr := e.st.GetPullByGlobalID(id); pr != nil {
				return e.execObject("PullRequest", pr, sels)
			}
			return nil
		case "__type":
			tName := asString(args["name"])
			if _, ok := schemaFields[tName]; !ok {
				if _, ok2 := schemaEnums[tName]; !ok2 {
					return nil
				}
			}
			return e.execObject("__Type", tName, sels)
		}

	case "Mutation":
		input := asMap(args["input"])
		switch field {
		case "createIssue":
			repoID := asString(input["repositoryId"])
			title := asString(input["title"])
			body := asString(input["body"])
			repo := e.st.GetRepoByGlobalID(repoID)
			if repo == nil {
				e.errors = append(e.errors, gqlError{Message: "Could not resolve to a Repository with the global id of '" + repoID + "'"})
				return nil
			}
			iss, err := e.st.CreateIssue(repo, title, body, nil)
			if err != nil {
				e.errors = append(e.errors, gqlError{Message: err.Error()})
				return nil
			}
			return e.execObject("CreateIssuePayload", map[string]any{
				"issue":            iss,
				"clientMutationId": input["clientMutationId"],
			}, sels)

		case "closeIssue":
			issueID := asString(input["issueId"])
			repo, iss := e.st.GetIssueByGlobalID(issueID)
			if repo == nil || iss == nil {
				e.errors = append(e.errors, gqlError{Message: "Could not resolve to an Issue with the global id of '" + issueID + "'"})
				return nil
			}
			closed := "closed"
			updated, err := e.st.UpdateIssue(repo, iss.Number, &closed, nil, nil, nil)
			if err != nil {
				e.errors = append(e.errors, gqlError{Message: err.Error()})
				return nil
			}
			return e.execObject("CloseIssuePayload", map[string]any{
				"issue":            updated,
				"clientMutationId": input["clientMutationId"],
			}, sels)

		case "createPullRequest":
			repoID := asString(input["repositoryId"])
			baseRef := asString(input["baseRefName"])
			headRef := asString(input["headRefName"])
			title := asString(input["title"])
			body := asString(input["body"])
			repo := e.st.GetRepoByGlobalID(repoID)
			if repo == nil {
				e.errors = append(e.errors, gqlError{Message: "Could not resolve to a Repository"})
				return nil
			}
			repo.GitMu.RLock()
			headOID, ok1 := repo.Refs["refs/heads/"+headRef]
			baseOID, ok2 := repo.Refs["refs/heads/"+baseRef]
			repo.GitMu.RUnlock()
			if !ok1 || !ok2 {
				e.errors = append(e.errors, gqlError{Message: "Head or base branch not found"})
				return nil
			}
			pr := e.st.CreatePullRequest(repo, title, body, headRef, git.FormatOID(headOID), baseRef, git.FormatOID(baseOID))
			return e.execObject("CreatePullRequestPayload", map[string]any{
				"pullRequest":      pr,
				"clientMutationId": input["clientMutationId"],
			}, sels)

		case "mergePullRequest":
			prID := asString(input["pullRequestId"])
			method := asString(input["mergeMethod"])
			if method == "" {
				method = "MERGE"
			}
			repo, pr := e.st.GetPullByGlobalID(prID)
			if repo == nil || pr == nil {
				e.errors = append(e.errors, gqlError{Message: "Could not resolve to a PullRequest"})
				return nil
			}
			_, merged, errMsg, _ := rest.MergePullRequest(repo, pr, strings.ToLower(method), "")
			if !merged {
				e.errors = append(e.errors, gqlError{Message: errMsg})
				return nil
			}
			return e.execObject("MergePullRequestPayload", map[string]any{
				"pullRequest":      pr,
				"clientMutationId": input["clientMutationId"],
			}, sels)

		case "addComment":
			subjID := asString(input["subjectId"])
			body := asString(input["body"])
			repo, iss := e.st.GetIssueByGlobalID(subjID)
			if repo == nil || iss == nil {
				e.errors = append(e.errors, gqlError{Message: "Could not resolve to an Issue"})
				return nil
			}
			c := e.st.AddComment(repo, iss.Number, body)
			if c == nil {
				e.errors = append(e.errors, gqlError{Message: "Failed to add comment"})
				return nil
			}
			return e.execObject("AddCommentPayload", map[string]any{
				"commentEdge":      map[string]any{"cursor": base64.RawURLEncoding.EncodeToString([]byte("comment:" + strconv.FormatInt(c.ID, 10))), "node": c},
				"clientMutationId": input["clientMutationId"],
			}, sels)
		}

	case "__Type":
		tName := obj.(string)
		switch field {
		case "name":
			return tName
		case "kind":
			if _, isEnum := schemaEnums[tName]; isEnum {
				return "ENUM"
			}
			return "OBJECT"
		case "fields":
			list := schemaFields[tName]
			if list == nil {
				return nil
			}
			out := make([]any, 0, len(list))
			for _, f := range list {
				out = append(out, e.execObject("__Field", f, sels))
			}
			return out
		case "enumValues":
			list := schemaEnums[tName]
			if list == nil {
				return nil
			}
			out := make([]any, 0, len(list))
			for _, ev := range list {
				out = append(out, e.execObject("__EnumValue", ev, sels))
			}
			return out
		}

	case "__Field":
		switch field {
		case "name":
			return obj.(string)
		case "isDeprecated":
			return false
		}

	case "__EnumValue":
		switch field {
		case "name":
			return obj.(string)
		case "isDeprecated":
			return false
		}

	case "User":
		m := obj.(map[string]any)
		return m[field]

	case "Repository":
		repo := obj.(*store.Repo)
		switch field {
		case "id":
			return repo.GlobalID
		case "databaseId":
			return repo.ID
		case "name":
			return repo.Name
		case "nameWithOwner":
			return repo.FullName
		case "description":
			repo.DataMu.RLock()
			d := repo.Description
			repo.DataMu.RUnlock()
			return d
		case "isPrivate":
			repo.DataMu.RLock()
			p := repo.Private
			repo.DataMu.RUnlock()
			return p
		case "owner":
			return e.execObject("User", map[string]any{
				"id":         store.EncodeGlobalID("User", store.ViewerID),
				"databaseId": store.ViewerID,
				"login":      repo.Owner,
				"name":       store.ViewerName,
			}, sels)
		case "defaultBranchRef":
			repo.DataMu.RLock()
			defBranch := repo.DefaultBranch
			repo.DataMu.RUnlock()
			repo.GitMu.RLock()
			oid, exists := repo.Refs["refs/heads/"+defBranch]
			repo.GitMu.RUnlock()
			if !exists {
				return nil
			}
			return e.execObject("Ref", map[string]any{
				"id":     store.EncodeGlobalID("Ref", repo.ID, 1),
				"name":   defBranch,
				"prefix": "refs/heads/",
				"oid":    git.FormatOID(oid),
			}, sels)
		case "viewerPermission":
			return "ADMIN"
		case "hasIssuesEnabled":
			return repo.HasIssues
		case "issue":
			num := int64(asInt(args["number"], 0))
			iss := repo.FastGetIssue(num)
			if iss == nil {
				return nil
			}
			return e.execObject("Issue", iss, sels)
		case "issues":
			return e.resolveIssuesConnection(repo, args, sels)
		case "pullRequest":
			num := int64(asInt(args["number"], 0))
			pr := e.st.GetPullRequest(repo, num)
			if pr == nil {
				return nil
			}
			return e.execObject("PullRequest", pr, sels)
		case "pullRequests":
			return e.resolvePullsConnection(repo, args, sels)
		}

	case "Ref":
		m := obj.(map[string]any)
		if field == "target" {
			oid := asString(m["oid"])
			return e.execObject("Commit", map[string]any{
				"id":             oid,
				"oid":            oid,
				"abbreviatedOid": oid[:minInt(7, len(oid))],
			}, sels)
		}
		return m[field]

	case "Commit", "PullRequestChangedFile":
		return obj.(map[string]any)[field]

	case "Issue":
		iss := obj.(*store.Issue)
		iss.RepoOwner = iss.RepoOwner // keep reference clean
		switch field {
		case "id":
			return iss.GlobalID
		case "databaseId":
			return iss.ID
		case "number":
			return iss.Number
		case "title":
			return iss.Title
		case "body":
			return iss.Body
		case "state":
			return strings.ToUpper(iss.State)
		case "createdAt":
			return iss.CreatedAt
		case "updatedAt":
			return iss.UpdatedAt
		case "closedAt":
			if iss.ClosedAt == "" {
				return nil
			}
			return iss.ClosedAt
		case "author":
			return e.execObject("User", map[string]any{"login": iss.AuthorLogin}, sels)
		}

	case "PullRequest":
		pr := obj.(*store.PullRequest)
		repo := e.st.GetRepo(pr.RepoOwner, pr.RepoName)
		if repo != nil {
			repo.DataMu.RLock()
			defer repo.DataMu.RUnlock()
		}
		switch field {
		case "id":
			return pr.GlobalID
		case "databaseId":
			return pr.ID
		case "number":
			return pr.Number
		case "title":
			return pr.Title
		case "body":
			return pr.Body
		case "headRefName":
			return pr.HeadRef
		case "baseRefName":
			return pr.BaseRef
		case "merged":
			return pr.Merged
		case "mergedAt":
			if pr.MergedAt == "" {
				return nil
			}
			return pr.MergedAt
		case "state":
			if pr.Merged {
				return "MERGED"
			}
			return strings.ToUpper(pr.State)
		case "mergeable":
			if pr.Merged || repo == nil {
				return "UNKNOWN"
			}
			headOID, ok1 := git.ParseOID(pr.HeadSHA)
			baseOID, ok2 := git.ParseOID(pr.BaseSHA)
			if !ok1 || !ok2 {
				return "UNKNOWN"
			}
			repo.GitMu.RLock()
			if curBase, ok := repo.Refs["refs/heads/"+pr.BaseRef]; ok {
				baseOID = curBase
			}
			if curHead, ok := repo.Refs["refs/heads/"+pr.HeadRef]; ok {
				headOID = curHead
			}
			_, conflict, err := git.MergeCommitsLocked(repo, baseOID, headOID)
			repo.GitMu.RUnlock()
			if err != nil {
				return "UNKNOWN"
			}
			if conflict {
				return "CONFLICTING"
			}
			return "MERGEABLE"
		case "mergeCommit":
			if pr.MergeCommitSHA == "" {
				return nil
			}
			return e.execObject("Commit", map[string]any{
				"oid":            pr.MergeCommitSHA,
				"abbreviatedOid": pr.MergeCommitSHA[:minInt(7, len(pr.MergeCommitSHA))],
			}, sels)
		case "files":
			return e.resolvePullFilesConnection(repo, pr, args, sels)
		case "author":
			return e.execObject("User", map[string]any{"login": pr.AuthorLogin}, sels)
		case "createdAt":
			return pr.CreatedAt
		case "updatedAt":
			return pr.UpdatedAt
		}

	case "CreateIssuePayload":
		m := obj.(map[string]any)
		if field == "issue" {
			return e.execObject("Issue", m["issue"], sels)
		}
		return m[field]
	case "CloseIssuePayload":
		m := obj.(map[string]any)
		if field == "issue" {
			return e.execObject("Issue", m["issue"], sels)
		}
		return m[field]
	case "CreatePullRequestPayload":
		m := obj.(map[string]any)
		if field == "pullRequest" {
			return e.execObject("PullRequest", m["pullRequest"], sels)
		}
		return m[field]
	case "MergePullRequestPayload":
		m := obj.(map[string]any)
		if field == "pullRequest" {
			return e.execObject("PullRequest", m["pullRequest"], sels)
		}
		return m[field]
	case "AddCommentPayload":
		m := obj.(map[string]any)
		if field == "commentEdge" {
			return e.execObject("IssueCommentEdge", m["commentEdge"], sels)
		}
		return m[field]
	case "IssueCommentEdge":
		m := obj.(map[string]any)
		if field == "node" {
			return e.execObject("IssueComment", m["node"], sels)
		}
		return m[field]
	case "IssueComment":
		c := obj.(*store.Comment)
		switch field {
		case "id":
			return c.GlobalID
		case "databaseId":
			return c.ID
		case "body":
			return c.Body
		case "createdAt":
			return c.CreatedAt
		case "updatedAt":
			return c.UpdatedAt
		case "author":
			return e.execObject("User", map[string]any{"login": c.AuthorLogin}, sels)
		}
	}
	return nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func encodeCursor(idx int) string {
	return base64.RawURLEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(idx)))
}

func decodeCursor(cur string) (int, bool) {
	if cur == "" {
		return -1, false
	}
	b, err := base64.RawURLEncoding.DecodeString(cur)
	if err != nil {
		return -1, false
	}
	s := string(b)
	if !strings.HasPrefix(s, "cursor:") {
		return -1, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(s, "cursor:"))
	if err != nil {
		return -1, false
	}
	return n, true
}

func (e *execCtx) resolveIssuesConnection(repo *store.Repo, args map[string]any, sels []selection) map[string]any {
	states := asStringSlice(args["states"])
	stateFilter := ""
	if len(states) == 1 {
		stateFilter = strings.ToLower(states[0])
	} else if len(states) == 0 {
		stateFilter = "all"
	}

	dir := "asc"
	if orderBy := asMap(args["orderBy"]); orderBy != nil {
		if strings.EqualFold(asString(orderBy["direction"]), "DESC") {
			dir = "desc"
		}
	}

	repo.DataMu.RLock()
	filtered := make([]*store.Issue, 0, len(repo.Issues))
	for _, iss := range repo.Issues {
		if stateFilter == "all" || stateFilter == "" {
			if len(states) > 0 {
				matched := false
				for _, st := range states {
					if strings.EqualFold(st, iss.State) {
						matched = true
						break
					}
				}
				if !matched {
					continue
				}
			}
			filtered = append(filtered, iss)
		} else if strings.EqualFold(iss.State, stateFilter) {
			filtered = append(filtered, iss)
		}
	}
	repo.DataMu.RUnlock()

	if dir == "desc" {
		sort.Slice(filtered, func(i, j int) bool {
			if filtered[i].CreatedSeq != filtered[j].CreatedSeq {
				return filtered[i].CreatedSeq > filtered[j].CreatedSeq
			}
			return filtered[i].Number > filtered[j].Number
		})
	} else {
		sort.Slice(filtered, func(i, j int) bool {
			if filtered[i].CreatedSeq != filtered[j].CreatedSeq {
				return filtered[i].CreatedSeq < filtered[j].CreatedSeq
			}
			return filtered[i].Number < filtered[j].Number
		})
	}

	start := 0
	if after := asString(args["after"]); after != "" {
		if idx, ok := decodeCursor(after); ok {
			start = idx + 1
		}
	}
	if start > len(filtered) {
		start = len(filtered)
	}
	first := asInt(args["first"], len(filtered))
	if first < 0 {
		first = 0
	}
	end := start + first
	if end > len(filtered) {
		end = len(filtered)
	}
	page := filtered[start:end]
	hasNext := end < len(filtered)
	hasPrev := start > 0
	var startCur, endCur any
	if len(page) > 0 {
		startCur = encodeCursor(start)
		endCur = encodeCursor(end - 1)
	}

	flat := e.flattenSelections("IssueConnection", sels)
	out := make(map[string]any, len(flat))
	for _, s := range flat {
		if !e.checkField("IssueConnection", s.name) {
			continue
		}
		key := keyOf(s)
		switch s.name {
		case "__typename":
			out[key] = "IssueConnection"
		case "totalCount":
			out[key] = len(filtered)
		case "nodes":
			nodes := make([]any, len(page))
			for i, iss := range page {
				nodes[i] = e.execObject("Issue", iss, s.sels)
			}
			out[key] = nodes
		case "edges":
			edges := make([]any, len(page))
			for i, iss := range page {
				edgeSels := e.flattenSelections("IssueEdge", s.sels)
				edgeObj := make(map[string]any, len(edgeSels))
				for _, es := range edgeSels {
					if !e.checkField("IssueEdge", es.name) {
						continue
					}
					switch es.name {
					case "__typename":
						edgeObj[keyOf(es)] = "IssueEdge"
					case "cursor":
						edgeObj[keyOf(es)] = encodeCursor(start + i)
					case "node":
						edgeObj[keyOf(es)] = e.execObject("Issue", iss, es.sels)
					}
				}
				edges[i] = edgeObj
			}
			out[key] = edges
		case "pageInfo":
			out[key] = e.execPageInfo(hasNext, hasPrev, startCur, endCur, s.sels)
		}
	}
	return out
}

func (e *execCtx) resolvePullsConnection(repo *store.Repo, args map[string]any, sels []selection) map[string]any {
	states := asStringSlice(args["states"])
	repo.DataMu.RLock()
	filtered := make([]*store.PullRequest, 0, len(repo.Pulls))
	for _, pr := range repo.Pulls {
		if len(states) > 0 {
			prState := strings.ToUpper(pr.State)
			if pr.Merged {
				prState = "MERGED"
			}
			matched := false
			for _, st := range states {
				if strings.EqualFold(st, prState) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		filtered = append(filtered, pr)
	}
	repo.DataMu.RUnlock()

	first := asInt(args["first"], len(filtered))
	if first < 0 {
		first = 0
	}
	end := minInt(first, len(filtered))
	page := filtered[:end]

	flat := e.flattenSelections("PullRequestConnection", sels)
	out := make(map[string]any, len(flat))
	for _, s := range flat {
		if !e.checkField("PullRequestConnection", s.name) {
			continue
		}
		key := keyOf(s)
		switch s.name {
		case "__typename":
			out[key] = "PullRequestConnection"
		case "totalCount":
			out[key] = len(filtered)
		case "nodes":
			nodes := make([]any, len(page))
			for i, pr := range page {
				nodes[i] = e.execObject("PullRequest", pr, s.sels)
			}
			out[key] = nodes
		case "pageInfo":
			var sc, ec any
			if len(page) > 0 {
				sc = encodeCursor(0)
				ec = encodeCursor(len(page) - 1)
			}
			out[key] = e.execPageInfo(end < len(filtered), false, sc, ec, s.sels)
		}
	}
	return out
}

func (e *execCtx) resolvePullFilesConnection(repo *store.Repo, pr *store.PullRequest, args map[string]any, sels []selection) map[string]any {
	var diffs []git.FileDiff
	if repo != nil {
		headOID, ok1 := git.ParseOID(pr.HeadSHA)
		baseOID, ok2 := git.ParseOID(pr.BaseSHA)
		if ok1 && ok2 {
			repo.GitMu.RLock()
			if curHead, ok := repo.Refs["refs/heads/"+pr.HeadRef]; ok {
				headOID = curHead
			}
			if curBase, ok := repo.Refs["refs/heads/"+pr.BaseRef]; ok {
				if lca, hasLCA := git.MergeBaseLocked(repo, curBase, headOID); hasLCA {
					baseOID = lca
				}
			}
			diffs, _ = git.DiffCommitsLocked(repo, baseOID, headOID)
			repo.GitMu.RUnlock()
		}
	}

	first := asInt(args["first"], len(diffs))
	if first < 0 {
		first = 0
	}
	end := minInt(first, len(diffs))
	page := diffs[:end]

	flat := e.flattenSelections("PullRequestChangedFileConnection", sels)
	out := make(map[string]any, len(flat))
	for _, s := range flat {
		if !e.checkField("PullRequestChangedFileConnection", s.name) {
			continue
		}
		key := keyOf(s)
		switch s.name {
		case "__typename":
			out[key] = "PullRequestChangedFileConnection"
		case "totalCount":
			out[key] = len(diffs)
		case "nodes":
			nodes := make([]any, len(page))
			for i, d := range page {
				ct := "MODIFIED"
				switch d.Status {
				case "added":
					ct = "ADDED"
				case "removed":
					ct = "DELETED"
				}
				nodes[i] = e.execObject("PullRequestChangedFile", map[string]any{
					"path":       d.Filename,
					"additions":  d.Additions,
					"deletions":  d.Deletions,
					"changeType": ct,
				}, s.sels)
			}
			out[key] = nodes
		case "pageInfo":
			out[key] = e.execPageInfo(end < len(diffs), false, nil, nil, s.sels)
		}
	}
	return out
}

func (e *execCtx) execPageInfo(hasNext, hasPrev bool, startCur, endCur any, sels []selection) map[string]any {
	flat := e.flattenSelections("PageInfo", sels)
	out := make(map[string]any, len(flat))
	for _, s := range flat {
		if !e.checkField("PageInfo", s.name) {
			continue
		}
		switch s.name {
		case "__typename":
			out[keyOf(s)] = "PageInfo"
		case "hasNextPage":
			out[keyOf(s)] = hasNext
		case "hasPreviousPage":
			out[keyOf(s)] = hasPrev
		case "startCursor":
			out[keyOf(s)] = startCur
		case "endCursor":
			out[keyOf(s)] = endCur
		}
	}
	return out
}

func init() {
	// Ensure PullRequestChangedFile resolves cleanly inside execObject.
	_ = unicode.IsLetter
}
