package cleanup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/cli/go-gh/v2/pkg/api"
	"gotest.tools/v3/assert"
)

// fakeAPI serves canned responses keyed by "METHOD path" and records the requests it has seen.
type fakeAPI struct {
	mu        sync.Mutex
	responses map[string]any
	errs      map[string]error
	requests  []string
}

func (f *fakeAPI) DoWithContext(_ context.Context, method, path string, _ io.Reader, response any) error {
	key := method + " " + path
	f.mu.Lock()
	f.requests = append(f.requests, key)
	f.mu.Unlock()
	if err, ok := f.errs[key]; ok {
		return err
	}
	body, ok := f.responses[key]
	if !ok {
		if method == http.MethodDelete {
			return nil
		}
		return fmt.Errorf("unexpected request %s", key)
	}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, response)
}

func (f *fakeAPI) deleted() []string {
	var ids []string
	for _, r := range f.requests {
		if id, ok := strings.CutPrefix(r, "DELETE notifications/threads/"); ok {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func notif(id, reason, subjectType, title, url string) map[string]any {
	return map[string]any{
		"id":         id,
		"reason":     reason,
		"subject":    map[string]any{"title": title, "url": url, "type": subjectType},
		"repository": map[string]any{"full_name": "acme/widgets"},
	}
}

const (
	listPath  = "GET notifications?all=true&is_done=false&per_page=50&page=1"
	mergedURL = "https://api.github.com/repos/acme/widgets/pulls/1"
	closedURL = "https://api.github.com/repos/acme/widgets/pulls/2"
	openURL   = "https://api.github.com/repos/acme/widgets/pulls/3"
	goneURL   = "https://api.github.com/repos/acme/widgets/pulls/4"
)

func newFake() *fakeAPI {
	return &fakeAPI{
		responses: map[string]any{
			listPath: []any{
				notif("1", "subscribed", "PullRequest", "merged pr", mergedURL),
				notif("2", "subscribed", "PullRequest", "closed pr", closedURL),
				notif("3", "subscribed", "PullRequest", "open pr", openURL),
				notif("4", "subscribed", "PullRequest", "gone pr", goneURL),
				notif("5", "ci_activity", "CheckSuite", "CI workflow run failed for main branch", ""),
				notif("6", "ci_activity", "CheckSuite", "CI workflow run succeeded for main branch", ""),
				notif("7", "mention", "Issue", "an issue", ""),
			},
			"GET " + mergedURL: pullRequest{State: "closed", Merged: true},
			"GET " + closedURL: pullRequest{State: "closed"},
			"GET " + openURL:   pullRequest{State: "open"},
		},
		errs: map[string]error{
			"GET " + goneURL: &api.HTTPError{StatusCode: http.StatusNotFound},
		},
	}
}

func run(t *testing.T, fake *fakeAPI, opts Options) (Summary, string, error) {
	t.Helper()
	var out bytes.Buffer
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	summary, err := New(fake, logger, &out, opts).Run(context.Background())
	return summary, out.String(), err
}

func TestRun(t *testing.T) {
	for _, tt := range []struct {
		name        string
		opts        Options
		wantDeleted []string
	}{
		{
			name:        "everything",
			opts:        Options{Merged: true, Closed: true, CIFailure: true},
			wantDeleted: []string{"1", "2", "5"},
		},
		{name: "merged only", opts: Options{Merged: true}, wantDeleted: []string{"1"}},
		{name: "closed only", opts: Options{Closed: true}, wantDeleted: []string{"2"}},
		{name: "ci only", opts: Options{CIFailure: true}, wantDeleted: []string{"5"}},
		{name: "dry run", opts: Options{Merged: true, Closed: true, CIFailure: true, DryRun: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFake()
			summary, out, err := run(t, fake, tt.opts)
			assert.NilError(t, err)
			assert.DeepEqual(t, fake.deleted(), tt.wantDeleted)
			assert.Equal(t, summary.Inspected, 7)
			wantCleaned := len(tt.wantDeleted)
			if tt.opts.DryRun {
				wantCleaned = 3
				assert.Assert(t, strings.Contains(out, "would mark done"), out)
			}
			assert.Equal(t, summary.Cleaned, wantCleaned)
		})
	}
}

func TestRun_pagination(t *testing.T) {
	fake := &fakeAPI{responses: map[string]any{
		"GET notifications?all=true&is_done=false&per_page=2&page=1": []any{
			notif("1", "ci_activity", "CheckSuite", "workflow run failed", ""),
			notif("2", "ci_activity", "CheckSuite", "workflow run failed", ""),
		},
		"GET notifications?all=true&is_done=false&per_page=2&page=2": []any{
			notif("3", "ci_activity", "CheckSuite", "workflow run failed", ""),
		},
	}}
	var out bytes.Buffer
	cleaner := New(fake, slog.New(slog.NewTextHandler(io.Discard, nil)), &out, Options{CIFailure: true})
	cleaner.pageSize = 2
	summary, err := cleaner.Run(context.Background())
	assert.NilError(t, err)
	assert.Equal(t, summary.Cleaned, 3)
	assert.DeepEqual(t, fake.deleted(), []string{"1", "2", "3"})
}

func TestRun_deleteFailureDoesNotStopOthers(t *testing.T) {
	fake := newFake()
	fake.errs["DELETE notifications/threads/1"] = errors.New("boom")
	summary, _, err := run(t, fake, Options{Merged: true, Closed: true, CIFailure: true})
	assert.ErrorContains(t, err, "boom")
	assert.Equal(t, summary.Cleaned, 2)
}

func TestRun_listFailure(t *testing.T) {
	fake := &fakeAPI{errs: map[string]error{listPath: errors.New("unauthorized")}}
	_, _, err := run(t, fake, Options{Merged: true})
	assert.ErrorContains(t, err, "unauthorized")
}
