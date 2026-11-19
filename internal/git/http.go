package git

import (
	"bytes"
	"encoding/hex"
	"errors"
	"sort"
	"strconv"
	"strings"

	"gitforge/internal/http1"
	"gitforge/internal/store"
)

const (
	pktKindData  = 1
	pktKindFlush = 2
	pktKindDelim = 3
	pktKindEnd   = 4

	maxSidebandPayload = 65515 // 65520 - 4 hex len - 1 band byte
)

type pkt struct {
	kind    int
	payload []byte
}

func parsePktLines(data []byte, stopAtFlushForTrailingPack bool) ([]pkt, []byte, error) {
	var pkts []pkt
	off := 0
	for off < len(data) {
		if off+4 > len(data) {
			return nil, nil, errors.New("truncated pkt-line header")
		}
		hdr := data[off : off+4]
		var pktLen int
		for i := 0; i < 4; i++ {
			c := hdr[i]
			var v int
			switch {
			case c >= '0' && c <= '9':
				v = int(c - '0')
			case c >= 'a' && c <= 'f':
				v = int(c-'a') + 10
			case c >= 'A' && c <= 'F':
				v = int(c-'A') + 10
			default:
				return nil, nil, errors.New("invalid hex pkt-line length")
			}
			pktLen = (pktLen << 4) | v
		}
		off += 4
		switch pktLen {
		case 0:
			pkts = append(pkts, pkt{kind: pktKindFlush})
			if stopAtFlushForTrailingPack {
				return pkts, data[off:], nil
			}
		case 1:
			pkts = append(pkts, pkt{kind: pktKindDelim})
		case 2:
			pkts = append(pkts, pkt{kind: pktKindEnd})
		case 3:
			return nil, nil, errors.New("invalid pkt-line length 0003")
		default:
			if pktLen > 65520 {
				return nil, nil, errors.New("pkt-line too large")
			}
			payLen := pktLen - 4
			if off+payLen > len(data) {
				return nil, nil, errors.New("truncated pkt-line payload")
			}
			pkts = append(pkts, pkt{kind: pktKindData, payload: data[off : off+payLen]})
			off += payLen
		}
	}
	return pkts, nil, nil
}

const hexChars = "0123456789abcdef"

func appendPktLen(dst []byte, totalLen int) []byte {
	return append(dst,
		hexChars[(totalLen>>12)&0x0f],
		hexChars[(totalLen>>8)&0x0f],
		hexChars[(totalLen>>4)&0x0f],
		hexChars[totalLen&0x0f],
	)
}

func appendPktData(dst []byte, payload []byte) []byte {
	dst = appendPktLen(dst, len(payload)+4)
	return append(dst, payload...)
}

func appendPktStr(dst []byte, s string) []byte {
	dst = appendPktLen(dst, len(s)+4)
	return append(dst, s...)
}

func appendSideband(dst []byte, band byte, data []byte) []byte {
	for len(data) > 0 {
		n := len(data)
		if n > maxSidebandPayload {
			n = maxSidebandPayload
		}
		dst = appendPktLen(dst, n+5)
		dst = append(dst, band)
		dst = append(dst, data[:n]...)
		data = data[n:]
	}
	return dst
}

// HandleGit serves Git Smart HTTP endpoints for /<owner>/<repo>.git/<suffix>.
func HandleGit(w *http1.ResponseWriter, r *http1.Request, st *store.Store, owner, repoName, suffix string) {
	repo := st.GetRepo(owner, repoName)
	if repo == nil {
		_ = w.WriteJSON(404, []byte(`{"message":"Not Found"}`+"\n"))
		return
	}

	switch {
	case r.Method == "GET" && suffix == "/info/refs":
		handleInfoRefs(w, r, repo)
	case r.Method == "POST" && suffix == "/git-upload-pack":
		handleUploadPack(w, r, repo)
	case r.Method == "POST" && suffix == "/git-receive-pack":
		handleReceivePack(w, r, repo)
	default:
		_ = w.WriteJSON(404, []byte(`{"message":"Not Found"}`+"\n"))
	}
}

func handleInfoRefs(w *http1.ResponseWriter, r *http1.Request, repo *store.Repo) {
	service := r.QueryParam("service")
	switch service {
	case "git-upload-pack":
		if strings.Contains(r.GitProtocol, "version=2") {
			_ = w.WriteRaw(repo.CachedUploadV2InfoRefs)
			return
		}
		body := buildV0UploadPackAdvertisement(repo)
		_ = w.WriteResponse(200, "application/x-git-upload-pack-advertisement", []http1.Header{
			{Name: "Cache-Control", Value: "no-cache"},
		}, body)
	case "git-receive-pack":
		if cached := repo.CachedRecvInfoRefs.Load(); cached != nil {
			_ = w.WriteRaw(*cached)
			return
		}
		body := buildV0ReceivePackAdvertisement(repo)
		raw := http1.BuildResponse(200, "application/x-git-receive-pack-advertisement", []http1.Header{
			{Name: "Cache-Control", Value: "no-cache"},
		}, "git-recv-adv", body)
		repo.CachedRecvInfoRefs.Store(&raw)
		_ = w.WriteRaw(raw)
	default:
		_ = w.WriteJSON(403, []byte(`{"message":"Unsupported service"}`+"\n"))
	}
}

func buildV0UploadPackAdvertisement(repo *store.Repo) []byte {
	repo.GitMu.RLock()
	defer repo.GitMu.RUnlock()

	caps := "multi_ack_detailed side-band-64k thin-pack ofs-delta shallow no-progress include-tag symref=HEAD:" + repo.HEAD + " agent=gitforge/1.0"
	var out []byte
	out = appendPktStr(out, "# service=git-upload-pack\n")
	out = append(out, "0000"...)

	if len(repo.Refs) == 0 {
		out = appendPktStr(out, "0000000000000000000000000000000000000000 capabilities^{}\x00"+caps+"\n")
		out = append(out, "0000"...)
		return out
	}

	first := true
	emit := func(oid [20]byte, refName string) {
		line := FormatOID(oid) + " " + refName
		if first {
			line += "\x00" + caps
			first = false
		}
		line += "\n"
		out = appendPktStr(out, line)
	}

	if headOID, ok := repo.Refs[repo.HEAD]; ok {
		emit(headOID, "HEAD")
	}

	names := make([]string, 0, len(repo.Refs))
	for name := range repo.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		oid := repo.Refs[name]
		emit(oid, name)
		if peeled, ok := PeelTagLocked(repo, oid); ok {
			emit(peeled, name+"^{}")
		}
	}
	out = append(out, "0000"...)
	return out
}

func buildV0ReceivePackAdvertisement(repo *store.Repo) []byte {
	repo.GitMu.RLock()
	defer repo.GitMu.RUnlock()

	const caps = "report-status report-status-v2 delete-refs side-band-64k ofs-delta agent=gitforge/1.0"
	out := make([]byte, 0, 512)
	out = appendPktStr(out, "# service=git-receive-pack\n")
	out = append(out, "0000"...)

	if len(repo.Refs) == 0 {
		out = appendPktStr(out, "0000000000000000000000000000000000000000 capabilities^{}\x00"+caps+"\n")
		out = append(out, "0000"...)
		return out
	}

	if len(repo.Refs) == 1 {
		for name, oid := range repo.Refs {
			var line [256]byte
			lb := line[:0]
			var hexBuf [40]byte
			hex.Encode(hexBuf[:], oid[:])
			lb = append(lb, hexBuf[:]...)
			lb = append(lb, ' ')
			lb = append(lb, name...)
			lb = append(lb, 0)
			lb = append(lb, caps...)
			lb = append(lb, '\n')
			out = appendPktData(out, lb)
		}
		out = append(out, "0000"...)
		return out
	}

	names := make([]string, 0, len(repo.Refs))
	for name := range repo.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	first := true
	for _, name := range names {
		oid := repo.Refs[name]
		var line [256]byte
		lb := line[:0]
		var hexBuf [40]byte
		hex.Encode(hexBuf[:], oid[:])
		lb = append(lb, hexBuf[:]...)
		lb = append(lb, ' ')
		lb = append(lb, name...)
		if first {
			lb = append(lb, 0)
			lb = append(lb, caps...)
			first = false
		}
		lb = append(lb, '\n')
		out = appendPktData(out, lb)
	}
	out = append(out, "0000"...)
	return out
}

func handleUploadPack(w *http1.ResponseWriter, r *http1.Request, repo *store.Repo) {
	pkts, _, err := parsePktLines(r.Body, false)
	if err != nil {
		_ = w.WriteJSON(400, []byte(`{"message":"Malformed pkt-line"}`+"\n"))
		return
	}

	isV2 := strings.Contains(r.GitProtocol, "version=2")
	var command string
	delimIdx := -1
	for i, p := range pkts {
		if p.kind == pktKindDelim {
			delimIdx = i
			break
		}
		if p.kind == pktKindData {
			line := strings.TrimSuffix(string(p.payload), "\n")
			if strings.HasPrefix(line, "command=") {
				command = strings.TrimPrefix(line, "command=")
				isV2 = true
			}
		}
	}

	if isV2 {
		args := pkts
		if delimIdx >= 0 {
			args = pkts[delimIdx+1:]
		}
		switch command {
		case "ls-refs":
			handleV2LsRefs(w, repo, args)
		case "fetch":
			handleV2Fetch(w, repo, args)
		default:
			_ = w.WriteJSON(400, []byte(`{"message":"Unsupported v2 command"}`+"\n"))
		}
		return
	}

	handleV0UploadPack(w, repo, pkts)
}

func handleV2LsRefs(w *http1.ResponseWriter, repo *store.Repo, args []pkt) {
	var peel, symrefs, unborn bool
	var prefixes []string
	for _, p := range args {
		if p.kind != pktKindData {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		switch {
		case line == "peel":
			peel = true
		case line == "symrefs":
			symrefs = true
		case line == "unborn":
			unborn = true
		case strings.HasPrefix(line, "ref-prefix "):
			prefixes = append(prefixes, strings.TrimPrefix(line, "ref-prefix "))
		}
	}

	defaultReq := peel && symrefs && unborn && len(prefixes) == 0
	if defaultReq {
		if cached := repo.CachedLsRefsDefault.Load(); cached != nil {
			_ = w.WriteRaw(*cached)
			return
		}
	}

	matchPrefix := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, pre := range prefixes {
			if strings.HasPrefix(name, pre) {
				return true
			}
		}
		return false
	}

	repo.GitMu.RLock()
	var out []byte
	if matchPrefix("HEAD") {
		if headOID, ok := repo.Refs[repo.HEAD]; ok {
			line := FormatOID(headOID) + " HEAD"
			if symrefs {
				line += " symref-target:" + repo.HEAD
			}
			line += "\n"
			out = appendPktStr(out, line)
		} else if unborn {
			line := "unborn HEAD"
			if symrefs {
				line += " symref-target:" + repo.HEAD
			}
			line += "\n"
			out = appendPktStr(out, line)
		}
	}

	names := make([]string, 0, len(repo.Refs))
	for name := range repo.Refs {
		if matchPrefix(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		oid := repo.Refs[name]
		line := FormatOID(oid) + " " + name
		if peel {
			if peeled, ok := PeelTagLocked(repo, oid); ok {
				line += " peeled:" + FormatOID(peeled)
			}
		}
		line += "\n"
		out = appendPktStr(out, line)
	}
	repo.GitMu.RUnlock()

	out = append(out, "0000"...)
	if defaultReq {
		raw := http1.BuildResponse(200, "application/x-git-upload-pack-result", []http1.Header{
			{Name: "Cache-Control", Value: "no-cache"},
		}, "git-ls-refs", out)
		repo.CachedLsRefsDefault.Store(&raw)
		_ = w.WriteRaw(raw)
		return
	}
	_ = w.WriteResponse(200, "application/x-git-upload-pack-result", []http1.Header{
		{Name: "Cache-Control", Value: "no-cache"},
	}, out)
}

type fetchParams struct {
	wants         [][20]byte
	haves         [][20]byte
	clientShallow map[[20]byte]bool
	depth         int
	done          bool
}

func parseFetchParams(args []pkt, isV0 bool) (fetchParams, error) {
	fp := fetchParams{
		clientShallow: make(map[[20]byte]bool),
	}
	for _, p := range args {
		if p.kind != pktKindData {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		if isV0 && strings.HasPrefix(line, "want ") {
			rest := strings.TrimPrefix(line, "want ")
			if sp := strings.IndexByte(rest, ' '); sp >= 0 {
				rest = rest[:sp]
			}
			oid, ok := ParseOID(rest)
			if !ok {
				return fp, errors.New("invalid want oid")
			}
			fp.wants = append(fp.wants, oid)
			continue
		}
		switch {
		case strings.HasPrefix(line, "want "):
			oid, ok := ParseOID(strings.TrimPrefix(line, "want "))
			if !ok {
				return fp, errors.New("invalid want oid")
			}
			fp.wants = append(fp.wants, oid)
		case strings.HasPrefix(line, "have "):
			oid, ok := ParseOID(strings.TrimPrefix(line, "have "))
			if !ok {
				return fp, errors.New("invalid have oid")
			}
			fp.haves = append(fp.haves, oid)
		case strings.HasPrefix(line, "shallow "):
			oid, ok := ParseOID(strings.TrimPrefix(line, "shallow "))
			if !ok {
				return fp, errors.New("invalid shallow oid")
			}
			fp.clientShallow[oid] = true
		case strings.HasPrefix(line, "deepen "):
			d, err := strconv.Atoi(strings.TrimPrefix(line, "deepen "))
			if err != nil || d < 0 {
				return fp, errors.New("invalid deepen")
			}
			fp.depth = d
		case line == "done":
			fp.done = true
		}
	}
	return fp, nil
}

type fetchResult struct {
	shallow   [][20]byte
	unshallow [][20]byte
	objects   []*store.GitObject
	ackOIDs   [][20]byte
}

func computeFetchObjectsLocked(repo *store.Repo, fp fetchParams) (fetchResult, error) {
	var res fetchResult

	for _, w := range fp.wants {
		if _, ok := repo.Objects[w]; !ok {
			return res, errors.New("wanted object not found")
		}
	}

	// Collect objects the client already has (respecting shallow boundaries).
	clientHas := make(map[[20]byte]bool)
	var markClientReachable func(root [20]byte)
	markClientReachable = func(root [20]byte) {
		type item struct {
			oid [20]byte
		}
		stack := [][20]byte{root}
		for len(stack) > 0 {
			oid := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if clientHas[oid] {
				continue
			}
			obj := repo.Objects[oid]
			if obj == nil {
				continue
			}
			clientHas[oid] = true
			switch obj.Type {
			case store.ObjCommit:
				ci := repo.Commits[oid]
				if ci != nil {
					stack = append(stack, ci.Tree)
					if !fp.clientShallow[oid] {
						for _, p := range ci.Parents {
							if !clientHas[p] {
								stack = append(stack, p)
							}
						}
					}
				}
			case store.ObjTree:
				entries, err := ParseTree(obj.Data)
				if err == nil {
					for _, e := range entries {
						if e.Mode != "160000" && !clientHas[e.OID] {
							stack = append(stack, e.OID)
						}
					}
				}
			case store.ObjTag:
				if tgt, _, _, err := ParseTag(obj.Data); err == nil && !clientHas[tgt] {
					stack = append(stack, tgt)
				}
			}
		}
	}
	for _, h := range fp.haves {
		if _, ok := repo.Objects[h]; ok {
			res.ackOIDs = append(res.ackOIDs, h)
			markClientReachable(h)
		}
	}

	// Collect wanted objects.
	wantedSet := make(map[[20]byte]bool)
	var sendList []*store.GitObject
	addObj := func(oid [20]byte) *store.GitObject {
		if wantedSet[oid] || clientHas[oid] {
			return nil
		}
		obj := repo.Objects[oid]
		if obj == nil {
			return nil
		}
		wantedSet[oid] = true
		sendList = append(sendList, obj)
		return obj
	}

	collectTreeAndBlobs := func(treeOID [20]byte) {
		stack := [][20]byte{treeOID}
		for len(stack) > 0 {
			oid := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			obj := addObj(oid)
			if obj == nil || obj.Type != store.ObjTree {
				continue
			}
			entries, err := ParseTree(obj.Data)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if e.Mode == "160000" {
					continue
				}
				if e.Mode == "40000" {
					if !wantedSet[e.OID] && !clientHas[e.OID] {
						stack = append(stack, e.OID)
					}
				} else {
					addObj(e.OID)
				}
			}
		}
	}

	if fp.depth > 0 {
		// Depth-limited commit traversal via BFS.
		type qNode struct {
			oid   [20]byte
			depth int
		}
		bestDepth := make(map[[20]byte]int)
		var q []qNode
		for _, w := range fp.wants {
			obj := repo.Objects[w]
			if obj == nil {
				continue
			}
			commitOID := w
			if obj.Type == store.ObjTag {
				addObj(w)
				peeled, ok := PeelTagLocked(repo, w)
				if !ok {
					continue
				}
				commitOID = peeled
			}
			if _, isCommit := repo.Commits[commitOID]; isCommit {
				if prev, seen := bestDepth[commitOID]; !seen || 0 < prev {
					bestDepth[commitOID] = 0
					q = append(q, qNode{oid: commitOID, depth: 0})
				}
			} else {
				// Direct tree/blob want
				if obj.Type == store.ObjTree {
					collectTreeAndBlobs(w)
				} else {
					addObj(w)
				}
			}
		}

		shallowSet := make(map[[20]byte]bool)
		unshallowSet := make(map[[20]byte]bool)
		includedCommits := make([][20]byte, 0, len(q))

		for len(q) > 0 {
			cur := q[0]
			q = q[1:]
			if cur.depth > bestDepth[cur.oid] {
				continue
			}
			includedCommits = append(includedCommits, cur.oid)
			ci := repo.Commits[cur.oid]
			if ci == nil {
				continue
			}
			if cur.depth+1 < fp.depth {
				if fp.clientShallow[cur.oid] && len(ci.Parents) > 0 {
					unshallowSet[cur.oid] = true
				}
				for _, p := range ci.Parents {
					nd := cur.depth + 1
					if prev, seen := bestDepth[p]; !seen || nd < prev {
						bestDepth[p] = nd
						q = append(q, qNode{oid: p, depth: nd})
					}
				}
			} else if len(ci.Parents) > 0 {
				shallowSet[cur.oid] = true
			}
		}

		for _, c := range includedCommits {
			if addObj(c) != nil || !clientHas[c] {
				if ci := repo.Commits[c]; ci != nil {
					collectTreeAndBlobs(ci.Tree)
				}
			}
		}

		for s := range shallowSet {
			res.shallow = append(res.shallow, s)
		}
		for u := range unshallowSet {
			res.unshallow = append(res.unshallow, u)
		}
		sort.Slice(res.shallow, func(i, j int) bool { return bytes.Compare(res.shallow[i][:], res.shallow[j][:]) < 0 })
		sort.Slice(res.unshallow, func(i, j int) bool { return bytes.Compare(res.unshallow[i][:], res.unshallow[j][:]) < 0 })
	} else {
		// Full reachability closure excluding clientHas.
		stack := make([][20]byte, 0, len(fp.wants))
		stack = append(stack, fp.wants...)
		for len(stack) > 0 {
			oid := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if wantedSet[oid] || clientHas[oid] {
				continue
			}
			obj := addObj(oid)
			if obj == nil {
				continue
			}
			switch obj.Type {
			case store.ObjCommit:
				ci := repo.Commits[oid]
				if ci != nil {
					stack = append(stack, ci.Tree)
					if !fp.clientShallow[oid] {
						for _, p := range ci.Parents {
							if !wantedSet[p] && !clientHas[p] {
								stack = append(stack, p)
							}
						}
					}
				}
			case store.ObjTree:
				entries, err := ParseTree(obj.Data)
				if err == nil {
					for _, e := range entries {
						if e.Mode != "160000" && !wantedSet[e.OID] && !clientHas[e.OID] {
							stack = append(stack, e.OID)
						}
					}
				}
			case store.ObjTag:
				if tgt, _, _, err := ParseTag(obj.Data); err == nil && !wantedSet[tgt] && !clientHas[tgt] {
					stack = append(stack, tgt)
				}
			}
		}
	}

	res.objects = sendList
	return res, nil
}

func handleV2Fetch(w *http1.ResponseWriter, repo *store.Repo, args []pkt) {
	fp, err := parseFetchParams(args, false)
	if err != nil {
		_ = w.WriteJSON(400, []byte(`{"message":"`+err.Error()+`"}`+"\n"))
		return
	}

	isShallowCloneHot := fp.depth == 1 && len(fp.wants) == 1 && len(fp.haves) == 0 && len(fp.clientShallow) == 0 && fp.done
	if isShallowCloneHot {
		if sc := repo.CachedShallowClone.Load(); sc != nil && sc.WantOID == fp.wants[0] {
			_ = w.WriteRaw(sc.RawHTTP)
			return
		}
	}

	repo.GitMu.RLock()
	res, err := computeFetchObjectsLocked(repo, fp)
	repo.GitMu.RUnlock()
	if err != nil {
		_ = w.WriteJSON(400, []byte(`{"message":"`+err.Error()+`"}`+"\n"))
		return
	}

	packBytes, err := WritePack(res.objects)
	if err != nil {
		_ = w.WriteJSON(500, []byte(`{"message":"Failed to build packfile"}`+"\n"))
		return
	}

	out := make([]byte, 0, len(packBytes)+256)
	if !fp.done {
		out = appendPktStr(out, "acknowledgments\n")
		if len(res.ackOIDs) == 0 {
			out = appendPktStr(out, "NAK\n")
		} else {
			for _, a := range res.ackOIDs {
				out = appendPktStr(out, "ACK "+FormatOID(a)+"\n")
			}
		}
		out = append(out, "0000"...)
		_ = w.WriteResponse(200, "application/x-git-upload-pack-result", []http1.Header{
			{Name: "Cache-Control", Value: "no-cache"},
		}, out)
		return
	}

	if len(res.shallow) > 0 || len(res.unshallow) > 0 {
		out = appendPktStr(out, "shallow-info\n")
		for _, s := range res.shallow {
			out = appendPktStr(out, "shallow "+FormatOID(s)+"\n")
		}
		for _, u := range res.unshallow {
			out = appendPktStr(out, "unshallow "+FormatOID(u)+"\n")
		}
		out = append(out, "0001"...)
	}

	out = appendPktStr(out, "packfile\n")
	out = appendSideband(out, 1, packBytes)
	out = append(out, "0000"...)

	if isShallowCloneHot {
		raw := http1.BuildResponse(200, "application/x-git-upload-pack-result", []http1.Header{
			{Name: "Cache-Control", Value: "no-cache"},
		}, "git-shallow-clone", out)
		repo.CachedShallowClone.Store(&store.ShallowCache{
			WantOID: fp.wants[0],
			RawHTTP: raw,
		})
		_ = w.WriteRaw(raw)
		return
	}

	_ = w.WriteResponse(200, "application/x-git-upload-pack-result", []http1.Header{
		{Name: "Cache-Control", Value: "no-cache"},
	}, out)
}

func handleV0UploadPack(w *http1.ResponseWriter, repo *store.Repo, pkts []pkt) {
	fp, err := parseFetchParams(pkts, true)
	if err != nil {
		_ = w.WriteJSON(400, []byte(`{"message":"`+err.Error()+`"}`+"\n"))
		return
	}

	repo.GitMu.RLock()
	res, err := computeFetchObjectsLocked(repo, fp)
	repo.GitMu.RUnlock()
	if err != nil {
		_ = w.WriteJSON(400, []byte(`{"message":"`+err.Error()+`"}`+"\n"))
		return
	}

	packBytes, err := WritePack(res.objects)
	if err != nil {
		_ = w.WriteJSON(500, []byte(`{"message":"Failed to build packfile"}`+"\n"))
		return
	}

	out := make([]byte, 0, len(packBytes)+256)
	for _, s := range res.shallow {
		out = appendPktStr(out, "shallow "+FormatOID(s)+"\n")
	}
	for _, u := range res.unshallow {
		out = appendPktStr(out, "unshallow "+FormatOID(u)+"\n")
	}
	if len(res.shallow) > 0 || len(res.unshallow) > 0 {
		out = append(out, "0000"...)
	}
	out = appendPktStr(out, "NAK\n")
	out = appendSideband(out, 1, packBytes)
	out = append(out, "0000"...)

	_ = w.WriteResponse(200, "application/x-git-upload-pack-result", []http1.Header{
		{Name: "Cache-Control", Value: "no-cache"},
	}, out)
}

type receiveCmd struct {
	oldOID [20]byte
	newOID [20]byte
	ref    string
}

func handleReceivePack(w *http1.ResponseWriter, r *http1.Request, repo *store.Repo) {
	pkts, packBytes, err := parsePktLines(r.Body, true)
	if err != nil {
		_ = w.WriteJSON(400, []byte(`{"message":"Malformed receive-pack request"}`+"\n"))
		return
	}

	var cmds []receiveCmd
	sideband := false
	for i, p := range pkts {
		if p.kind != pktKindData {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		if i == 0 {
			if nul := strings.IndexByte(line, 0); nul >= 0 {
				caps := line[nul+1:]
				line = line[:nul]
				if strings.Contains(caps, "side-band-64k") {
					sideband = true
				}
			}
		}
		parts := strings.SplitN(line, " ", 3)
		if len(parts) != 3 {
			_ = w.WriteJSON(400, []byte(`{"message":"Malformed command line"}`+"\n"))
			return
		}
		oldOID, ok1 := ParseOID(parts[0])
		newOID, ok2 := ParseOID(parts[1])
		if !ok1 || !ok2 {
			_ = w.WriteJSON(400, []byte(`{"message":"Invalid oid in push command"}`+"\n"))
			return
		}
		cmds = append(cmds, receiveCmd{oldOID: oldOID, newOID: newOID, ref: parts[2]})
	}

	var unpackErr error
	repo.GitMu.Lock()
	if len(packBytes) > 0 {
		unpackErr = ParsePackAndStoreLocked(repo, packBytes)
	}

	type refResult struct {
		ref    string
		ok     bool
		reason string
	}
	results := make([]refResult, len(cmds))
	anyChanged := false

	if unpackErr == nil {
		for i, c := range cmds {
			curOID, exists := repo.Refs[c.ref]
			if c.newOID == ZeroOID {
				// Delete ref
				if !exists {
					results[i] = refResult{ref: c.ref, ok: false, reason: "missing ref"}
					continue
				}
				if c.oldOID != ZeroOID && c.oldOID != curOID {
					results[i] = refResult{ref: c.ref, ok: false, reason: "non-fast-forward"}
					continue
				}
				delete(repo.Refs, c.ref)
				results[i] = refResult{ref: c.ref, ok: true}
				anyChanged = true
				continue
			}

			// Create or update ref: verify newOID exists in store.
			if _, ok := repo.Objects[c.newOID]; !ok {
				results[i] = refResult{ref: c.ref, ok: false, reason: "missing object"}
				continue
			}

			if c.oldOID == ZeroOID {
				if exists && curOID != c.newOID {
					results[i] = refResult{ref: c.ref, ok: false, reason: "non-fast-forward"}
					continue
				}
				repo.Refs[c.ref] = c.newOID
				results[i] = refResult{ref: c.ref, ok: true}
				anyChanged = true
				continue
			}

			if !exists || curOID != c.oldOID || !IsAncestorLocked(repo, c.oldOID, c.newOID) {
				results[i] = refResult{ref: c.ref, ok: false, reason: "non-fast-forward"}
				continue
			}

			repo.Refs[c.ref] = c.newOID
			results[i] = refResult{ref: c.ref, ok: true}
			anyChanged = true
		}
		if anyChanged {
			repo.InvalidateGitCaches()
		}
	}
	repo.GitMu.Unlock()

	var report []byte
	if unpackErr != nil {
		report = appendPktStr(report, "unpack error "+unpackErr.Error()+"\n")
		for _, c := range cmds {
			report = appendPktStr(report, "ng "+c.ref+" unpacker error\n")
		}
	} else {
		report = appendPktStr(report, "unpack ok\n")
		for _, res := range results {
			if res.ok {
				report = appendPktStr(report, "ok "+res.ref+"\n")
			} else {
				report = appendPktStr(report, "ng "+res.ref+" "+res.reason+"\n")
			}
		}
	}
	report = append(report, "0000"...)

	var body []byte
	if sideband {
		body = make([]byte, 0, len(report)+16)
		body = appendSideband(body, 1, report)
		body = append(body, "0000"...)
	} else {
		body = report
	}

	_ = w.WriteResponse(200, "application/x-git-receive-pack-result", []http1.Header{
		{Name: "Cache-Control", Value: "no-cache"},
	}, body)
}
