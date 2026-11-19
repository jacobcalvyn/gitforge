package store

import (
	"bytes"
	"testing"
)

func TestMarshalIssuePlainJSONMatchesMarshalIssueJSON(t *testing.T) {
	cases := []struct {
		id     int64
		num    int64
		title  string
		body   string
	}{
		{id: 1, num: 1, title: "a", body: ""},
		{id: 10001, num: 2, title: "Slow build [bench 1]", body: "One sentence body."},
		{id: 9999999999, num: 123456, title: "Title 123", body: "Body text without escapes or quotes"},
	}
	for _, tc := range cases {
		iss := &Issue{
			ID:     tc.id,
			Number: tc.num,
			Title:  tc.title,
			Body:   tc.body,
			State:  "open",
		}
		got := MarshalIssuePlainJSON(iss, nil)
		want := append(MarshalIssueJSON(iss, nil), '\n')
		if !bytes.Equal(got, want) {
			t.Fatalf("MarshalIssuePlainJSON mismatch for %+v:\ngot:  %q\nwant: %q", tc, got, want)
		}
		if gotLen := IssuePlainJSONLen(iss); gotLen != len(got) {
			t.Fatalf("IssuePlainJSONLen=%d != len(got)=%d for %+v", gotLen, len(got), tc)
		}
	}
}
