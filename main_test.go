package main

import (
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// fake serves both a minimal JMAP server and the paperless upload endpoint.
type fake struct {
	emails       map[string]*email
	uploads      []string // "name=content tags=[...]"
	uploadStatus int
}

func newFake(t *testing.T) (*fake, config) {
	f := &fake{uploadStatus: 200, emails: map[string]*email{
		"e1": {ID: "e1", Subject: "pdf and logo", MailboxIDs: set("inbox", "pl"), Attachments: []attachment{
			{"b1", "invoice.pdf", "application/pdf"}, {"b2", "logo.png", "image/png"}}},
		"e2": {ID: "e2", Subject: "only in label", MailboxIDs: set("pl"), Attachments: []attachment{
			{"b3", "scan.PDF", "application/octet-stream"}}},
		"e3": {ID: "e3", Subject: "partial failure", MailboxIDs: set("pl"), Attachments: []attachment{
			{"b4", "ok.pdf", "application/pdf"}, {"b5", "bad.pdf", "application/pdf"}}},
		"e4": {ID: "e4", Subject: "no pdf", MailboxIDs: set("inbox", "pl"), Attachments: []attachment{
			{"b6", "photo.png", "image/png"}}},
		"e5": {ID: "e5", Subject: "not labelled", MailboxIDs: set("inbox"), Attachments: []attachment{
			{"b7", "other.pdf", "application/pdf"}}},
	}}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(r.URL.Path, "/api/documents/") {
			if auth != "Token pt" {
				w.WriteHeader(401)
				return
			}
			file, hdr, _ := r.FormFile("document")
			data, _ := io.ReadAll(file)
			f.uploads = append(f.uploads, hdr.Filename+"="+string(data)+" tags="+strings.Join(r.MultipartForm.Value["tags"], ","))
			status := f.uploadStatus
			if hdr.Filename == "bad.pdf" {
				status = 400
			}
			w.WriteHeader(status)
			return
		}
		if auth != "Bearer jt" {
			w.WriteHeader(401)
			return
		}
		switch {
		case r.URL.Path == "/session":
			json.NewEncoder(w).Encode(map[string]any{
				"apiUrl":          srv.URL + "/jmap",
				"downloadUrl":     srv.URL + "/download/{accountId}/{blobId}/{name}?type={type}",
				"primaryAccounts": map[string]string{"urn:ietf:params:jmap:mail": "acc"},
			})
		case strings.HasPrefix(r.URL.Path, "/download/acc/"):
			io.WriteString(w, "blob:"+strings.Split(r.URL.Path, "/")[3])
		case r.URL.Path == "/jmap":
			var req struct{ MethodCalls [][]json.RawMessage }
			json.NewDecoder(r.Body).Decode(&req)
			var method string
			json.Unmarshal(req.MethodCalls[0][0], &method)
			json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{[]any{method, f.handle(t, method, req.MethodCalls[0][1]), "0"}}})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return f, config{
		sessionURL: srv.URL + "/session", jmapToken: "jt",
		paperlessURL: srv.URL, paperlessToken: "pt",
		tagIDs: []string{"7", "9"}, allowedTypes: []string{"application/pdf"},
		label: "paperless", failedLabel: "paperless/failed",
	}
}

func (f *fake) handle(t *testing.T, method string, raw json.RawMessage) any {
	var args struct {
		Filter struct{ InMailbox string }
		IDs    []string
		Update map[string]map[string]any
	}
	json.Unmarshal(raw, &args)
	switch method {
	case "Mailbox/get":
		return map[string]any{"list": []map[string]any{
			{"id": "inbox", "name": "Inbox", "role": "inbox"},
			{"id": "arch", "name": "Archive", "role": "archive"},
			{"id": "pl", "name": "paperless"},
			{"id": "plf", "name": "failed", "parentId": "pl"},
		}}
	case "Email/query":
		var ids []string
		for id, e := range f.emails {
			if e.MailboxIDs[args.Filter.InMailbox] {
				ids = append(ids, id)
			}
		}
		slices.Sort(ids)
		return map[string]any{"ids": ids}
	case "Email/get":
		var list []*email
		for _, id := range args.IDs {
			list = append(list, f.emails[id])
		}
		return map[string]any{"list": list}
	case "Email/set":
		notUpdated := map[string]any{}
		for id, patch := range args.Update {
			boxes := maps.Clone(f.emails[id].MailboxIDs)
			for k, v := range patch {
				box := strings.TrimPrefix(k, "mailboxIds/")
				if v == nil {
					delete(boxes, box)
				} else {
					boxes[box] = true
				}
			}
			if len(boxes) == 0 {
				notUpdated[id] = map[string]string{"type": "invalidProperties"}
				continue
			}
			f.emails[id].MailboxIDs = boxes
		}
		return map[string]any{"notUpdated": notUpdated}
	}
	t.Fatalf("unexpected method %s", method)
	return nil
}

func set(ids ...string) map[string]bool {
	m := map[string]bool{}
	for _, id := range ids {
		m[id] = true
	}
	return m
}

func TestRun(t *testing.T) {
	f, cfg := newFake(t)
	if err := run(cfg, http.DefaultClient); err != nil {
		t.Fatal(err)
	}
	wantUploads := []string{
		"invoice.pdf=blob:b1 tags=7,9",
		"scan.PDF=blob:b3 tags=7,9", // octet-stream allowed by extension
		"ok.pdf=blob:b4 tags=7,9",
		"bad.pdf=blob:b5 tags=7,9", // rejected with 400, but still attempted
	}
	if !reflect.DeepEqual(f.uploads, wantUploads) {
		t.Errorf("uploads = %q, want %q", f.uploads, wantUploads)
	}
	want := map[string]map[string]bool{
		"e1": set("inbox"),
		"e2": set("arch"), // would be in no mailbox, so archived
		"e3": set("plf"),
		"e4": set("inbox", "plf"),
		"e5": set("inbox"),
	}
	for id, boxes := range want {
		if got := f.emails[id].MailboxIDs; !reflect.DeepEqual(got, boxes) {
			t.Errorf("%s mailboxes = %v, want %v", id, got, boxes)
		}
	}
}

func TestRunAbortsWhenPaperlessDown(t *testing.T) {
	f, cfg := newFake(t)
	f.uploadStatus = 503
	if err := run(cfg, http.DefaultClient); err == nil {
		t.Fatal("expected error")
	}
	if got := f.emails["e1"].MailboxIDs; !reflect.DeepEqual(got, set("inbox", "pl")) {
		t.Errorf("e1 mailboxes = %v, want untouched", got)
	}
}
