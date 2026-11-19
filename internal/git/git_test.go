package git

import (
	"bytes"
	"testing"

	"gitforge/internal/store"
)

func TestTreeSortAndPackRoundTrip(t *testing.T) {
	st := store.New()
	r, _ := st.CreateRepo(store.ViewerLogin, "test", "", true, "main")

	r.GitMu.Lock()
	b1, err := PutObjectLocked(r, store.ObjBlob, []byte("hello\n"))
	if err != nil {
		t.Fatal(err)
	}
	b2, err := PutObjectLocked(r, store.ObjBlob, []byte("nested\n"))
	if err != nil {
		t.Fatal(err)
	}
	subTree, err := WriteTreeFromFlatLocked(r, map[string]FlatEntry{
		"a.txt": {Mode: "100644", OID: b2},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Git rule: "foo-bar" sorts before "foo/" because '-' (0x2d) < '/' (0x2f).
	rawTree := BuildTree([]TreeEntry{
		{Mode: "40000", Name: "foo", OID: subTree},
		{Mode: "100644", Name: "foo-bar", OID: b1},
	})
	entries, err := ParseTree(rawTree)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name != "foo-bar" || entries[1].Name != "foo" {
		t.Fatalf("wrong git tree sort order: %+v", entries)
	}

	treeOID, err := PutObjectLocked(r, store.ObjTree, rawTree)
	if err != nil {
		t.Fatal(err)
	}
	commitBytes := BuildCommit(treeOID, nil, store.ViewerName, store.ViewerEmail, 1735689600, "initial")
	commitOID, err := PutObjectLocked(r, store.ObjCommit, commitBytes)
	if err != nil {
		t.Fatal(err)
	}

	pack, err := WritePack([]*store.GitObject{
		r.Objects[b1],
		r.Objects[b2],
		r.Objects[subTree],
		r.Objects[treeOID],
		r.Objects[commitOID],
	})
	r.GitMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	r2, _ := st.CreateRepo(store.ViewerLogin, "test2", "", true, "main")
	r2.GitMu.Lock()
	if err := ParsePackAndStoreLocked(r2, pack); err != nil {
		t.Fatal(err)
	}
	got := r2.Objects[commitOID]
	r2.GitMu.Unlock()
	if got == nil || !bytes.Equal(got.Data, commitBytes) {
		t.Fatalf("pack roundtrip failed")
	}
}
