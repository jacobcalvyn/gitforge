package graphql

import (
	"encoding/json"
	"testing"

	"gitforge/internal/store"
)

func TestGraphQLCoreAndPagination(t *testing.T) {
	st := store.New()
	repo, _ := st.CreateRepo(store.ViewerLogin, "demo", "desc", true, "main")
	for i := 1; i <= 7; i++ {
		if _, err := st.CreateIssue(repo, "Issue", "Body", nil); err != nil {
			t.Fatal(err)
		}
	}

	// 1. Unknown field yields error naming the field.
	res := executeOperation(requestEnvelope{
		Query: `query ViewerInvalidField { viewer { login noSuchFieldOnViewer } }`,
	}, st)
	if len(res.Errors) == 0 || res.Errors[0].Message == "" {
		t.Fatalf("expected error for unknown field")
	}

	// 2. Introspection of Issue and SearchType.
	res2 := executeOperation(requestEnvelope{
		Query: `query Introspect { Issue: __type(name: "Issue") { fields(includeDeprecated: true) { name } } SearchType: __type(name: "SearchType") { enumValues(includeDeprecated: true) { name } } }`,
	}, st)
	if len(res2.Errors) > 0 {
		t.Fatalf("unexpected errors: %+v", res2.Errors)
	}
	b, _ := json.Marshal(res2.Data)
	if string(b) == "" {
		t.Fatal("empty introspection")
	}

	// 3. Cursor pagination: 5 + 2
	p1 := executeOperation(requestEnvelope{
		Query: `query P($owner: String!, $name: String!, $first: Int!, $after: String) { repository(owner: $owner, name: $name) { issues(first: $first, after: $after) { nodes { number } pageInfo { hasNextPage endCursor } } } }`,
		Variables: map[string]any{
			"owner": store.ViewerLogin,
			"name":  "demo",
			"first": float64(5),
			"after": nil,
		},
	}, st)
	if len(p1.Errors) > 0 {
		t.Fatalf("page 1 error: %+v", p1.Errors)
	}
	repoMap := p1.Data.(map[string]any)["repository"].(map[string]any)
	conn1 := repoMap["issues"].(map[string]any)
	if len(conn1["nodes"].([]any)) != 5 {
		t.Fatalf("expected 5 nodes, got %d", len(conn1["nodes"].([]any)))
	}
	pi1 := conn1["pageInfo"].(map[string]any)
	if pi1["hasNextPage"] != true {
		t.Fatal("expected hasNextPage=true")
	}

	p2 := executeOperation(requestEnvelope{
		Query: `query P($owner: String!, $name: String!, $first: Int!, $after: String) { repository(owner: $owner, name: $name) { issues(first: $first, after: $after) { nodes { number } pageInfo { hasNextPage endCursor } } } }`,
		Variables: map[string]any{
			"owner": store.ViewerLogin,
			"name":  "demo",
			"first": float64(5),
			"after": pi1["endCursor"],
		},
	}, st)
	conn2 := p2.Data.(map[string]any)["repository"].(map[string]any)["issues"].(map[string]any)
	if len(conn2["nodes"].([]any)) != 2 {
		t.Fatalf("expected 2 nodes on page 2, got %d", len(conn2["nodes"].([]any)))
	}
	if conn2["pageInfo"].(map[string]any)["hasNextPage"] != false {
		t.Fatal("expected hasNextPage=false on page 2")
	}
}
