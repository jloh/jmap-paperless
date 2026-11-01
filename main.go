// Command jmap-paperless uploads attachments from emails in one JMAP mailbox/label
// to paperless-ngx, then removes the label. It runs once and exits; schedule it externally.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// batchSize caps emails per run; the next run picks up the rest.
const batchSize = 50

type config struct {
	sessionURL, jmapToken        string
	paperlessURL, paperlessToken string
	tagIDs, allowedTypes         []string
	label, failedLabel           string
}

type attachment struct{ BlobID, Name, Type string }

type email struct {
	ID          string
	Subject     string
	MailboxIDs  map[string]bool
	Attachments []attachment
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))
	cfg, err := loadConfig()
	if err == nil {
		err = run(cfg, &http.Client{Timeout: 2 * time.Minute})
	}
	if err != nil {
		slog.Error("run failed", "err", err)
		os.Exit(1)
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func list(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' })
}

func loadConfig() (config, error) {
	cfg := config{
		sessionURL:     env("JMAP_SESSION_URL", "https://api.fastmail.com/jmap/session"),
		jmapToken:      os.Getenv("JMAP_TOKEN"),
		paperlessURL:   strings.TrimSuffix(os.Getenv("PAPERLESS_URL"), "/"),
		paperlessToken: os.Getenv("PAPERLESS_TOKEN"),
		tagIDs:         list(os.Getenv("PAPERLESS_TAG_IDS")),
		allowedTypes:   list(strings.ToLower(env("ALLOWED_TYPES", "application/pdf"))),
		label:          env("LABEL", "paperless"),
		failedLabel:    env("FAILED_LABEL", "paperless/failed"),
	}
	if cfg.jmapToken == "" || cfg.paperlessURL == "" || cfg.paperlessToken == "" {
		return cfg, errors.New("JMAP_TOKEN, PAPERLESS_URL and PAPERLESS_TOKEN are required")
	}
	return cfg, nil
}

func run(cfg config, client *http.Client) error {
	j, err := newJMAP(client, cfg.sessionURL, cfg.jmapToken)
	if err != nil {
		return fmt.Errorf("jmap session: %w", err)
	}
	boxes, err := j.mailboxIDs()
	if err != nil {
		return err
	}
	labelID, failedID, archiveID := boxes[cfg.label], boxes[cfg.failedLabel], boxes["role:archive"]
	if labelID == "" || failedID == "" || archiveID == "" {
		return fmt.Errorf("mailboxes %q, %q and an archive mailbox must all exist", cfg.label, cfg.failedLabel)
	}

	var q struct{ IDs []string }
	if err := j.call("Email/query", map[string]any{"filter": map[string]string{"inMailbox": labelID}, "limit": batchSize}, &q); err != nil {
		return err
	}
	if len(q.IDs) == 0 {
		slog.Info("no emails to process")
		return nil
	}
	var g struct{ List []email }
	if err := j.call("Email/get", map[string]any{"ids": q.IDs, "properties": []string{"subject", "mailboxIds", "attachments"}}, &g); err != nil {
		return err
	}

	for _, e := range g.List {
		ok, err := ingest(cfg, j, e)
		if err != nil {
			return fmt.Errorf("email %q: %w", e.Subject, err)
		}
		patch := map[string]any{"mailboxIds/" + labelID: nil}
		if !ok {
			patch["mailboxIds/"+failedID] = true
		} else if len(e.MailboxIDs) == 1 {
			// JMAP requires every email to be in at least one mailbox.
			patch["mailboxIds/"+archiveID] = true
		}
		var s struct{ NotUpdated map[string]any }
		if err := j.call("Email/set", map[string]any{"update": map[string]any{e.ID: patch}}, &s); err != nil {
			return err
		}
		if len(s.NotUpdated) > 0 {
			return fmt.Errorf("updating email %q: %v", e.Subject, s.NotUpdated[e.ID])
		}
		slog.Info("processed email", "subject", e.Subject, "ok", ok)
	}
	return nil
}

// ingest uploads every allowed attachment of e. It returns false when the email
// should be marked failed, and an error only when the whole run should abort.
func ingest(cfg config, j *jmap, e email) (bool, error) {
	ok, found := true, false
	for _, a := range e.Attachments {
		if !slices.Contains(cfg.allowedTypes, strings.ToLower(a.Type)) &&
			!slices.Contains(cfg.allowedTypes, mime.TypeByExtension(filepath.Ext(a.Name))) {
			continue
		}
		found = true
		if err := transfer(cfg, j, a); err != nil {
			if !emailError(err) {
				return false, err
			}
			slog.Warn("attachment failed", "subject", e.Subject, "name", a.Name, "err", err)
			ok = false
		}
	}
	if !found {
		slog.Warn("no allowed attachments", "subject", e.Subject)
	}
	return ok && found, nil
}

// transfer downloads one attachment from JMAP and uploads it to paperless.
func transfer(cfg config, j *jmap, a attachment) error {
	name := a.Name
	if name == "" {
		name = a.BlobID
	}
	u := strings.NewReplacer(
		"{accountId}", j.accountID, "{blobId}", a.BlobID,
		"{name}", url.PathEscape(name), "{type}", url.QueryEscape(a.Type),
	).Replace(j.downloadURL)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return err
	}
	data, err := j.do(req)
	if err != nil {
		return fmt.Errorf("download %q: %w", name, err)
	}

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	fw, _ := w.CreateFormFile("document", name) // writes to a bytes.Buffer cannot fail
	fw.Write(data)
	for _, t := range cfg.tagIDs {
		w.WriteField("tags", t)
	}
	w.Close()
	req, err = http.NewRequest("POST", cfg.paperlessURL+"/api/documents/post_document/", &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Token "+cfg.paperlessToken)
	if _, err := do(j.client, req); err != nil {
		return fmt.Errorf("upload %q: %w", name, err)
	}
	return nil
}

type statusError struct {
	code int
	body string
}

func (e *statusError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.code, e.body) }

// emailError reports whether err is specific to one attachment (a 4xx other than
// auth), as opposed to a sign the service is down or misconfigured.
func emailError(err error) bool {
	var se *statusError
	return errors.As(err, &se) && se.code >= 400 && se.code < 500 && se.code != 401 && se.code != 403
}

func do(client *http.Client, req *http.Request) ([]byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, &statusError{resp.StatusCode, string(body[:min(len(body), 200)])}
	}
	return body, nil
}

type jmap struct {
	client                                *http.Client
	token, apiURL, downloadURL, accountID string
}

func newJMAP(client *http.Client, sessionURL, token string) (*jmap, error) {
	j := &jmap{client: client, token: token}
	req, err := http.NewRequest("GET", sessionURL, nil)
	if err != nil {
		return nil, err
	}
	body, err := j.do(req)
	if err != nil {
		return nil, err
	}
	var s struct {
		APIURL          string            `json:"apiUrl"`
		DownloadURL     string            `json:"downloadUrl"`
		PrimaryAccounts map[string]string `json:"primaryAccounts"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, err
	}
	j.apiURL, j.downloadURL, j.accountID = s.APIURL, s.DownloadURL, s.PrimaryAccounts["urn:ietf:params:jmap:mail"]
	if j.accountID == "" {
		return nil, errors.New("no mail account in session")
	}
	return j, nil
}

func (j *jmap) do(req *http.Request) ([]byte, error) {
	req.Header.Set("Authorization", "Bearer "+j.token)
	return do(j.client, req)
}

// call makes a single JMAP method call and decodes its arguments into out.
func (j *jmap) call(method string, args map[string]any, out any) error {
	args["accountId"] = j.accountID
	body, err := json.Marshal(map[string]any{
		"using":       []string{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"},
		"methodCalls": []any{[]any{method, args, "0"}},
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", j.apiURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := j.do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	var r struct{ MethodResponses [][]json.RawMessage }
	if err := json.Unmarshal(resp, &r); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	if len(r.MethodResponses) != 1 || len(r.MethodResponses[0]) != 3 {
		return fmt.Errorf("%s: unexpected response %s", method, resp)
	}
	if string(r.MethodResponses[0][0]) == `"error"` {
		return fmt.Errorf("%s: %s", method, r.MethodResponses[0][1])
	}
	return json.Unmarshal(r.MethodResponses[0][1], out)
}

// mailboxIDs returns mailbox IDs keyed by slash-joined path (e.g. "paperless/failed")
// and by role (e.g. "role:archive").
func (j *jmap) mailboxIDs() (map[string]string, error) {
	var r struct {
		List []struct{ ID, Name, ParentID, Role string }
	}
	if err := j.call("Mailbox/get", map[string]any{"properties": []string{"name", "parentId", "role"}}, &r); err != nil {
		return nil, err
	}
	byID := map[string]int{}
	for i, m := range r.List {
		byID[m.ID] = i
	}
	var path func(i int) string
	path = func(i int) string {
		m := r.List[i]
		if p, ok := byID[m.ParentID]; ok {
			return path(p) + "/" + m.Name
		}
		return m.Name
	}
	out := map[string]string{}
	for i, m := range r.List {
		out[path(i)] = m.ID
		if m.Role != "" {
			out["role:"+m.Role] = m.ID
		}
	}
	return out, nil
}
