// Package cleanup marks GitHub notifications as done when they are no longer actionable: pull requests that have been
// closed or merged, and CI failures.
package cleanup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/cli/go-gh/v2/pkg/api"
)

const (
	// pageSize is the maximum page size of the notifications API. Larger values are silently capped, which would make
	// a full page look like the last one.
	pageSize = 50
	// defaultConcurrency is how many notifications are processed at the same time.
	defaultConcurrency = 8
	// subjectPullRequest is the subject type of pull request notifications.
	subjectPullRequest = "PullRequest"
	// subjectCheckSuite is the subject type of CI notifications.
	subjectCheckSuite = "CheckSuite"
	// reasonCIActivity is the reason of CI notifications.
	reasonCIActivity = "ci_activity"
)

// Category is why a notification is cleaned up.
type Category string

// The categories of notifications that can be cleaned up.
const (
	// CategoryMerged is a notification for a merged pull request.
	CategoryMerged Category = "merged"
	// CategoryClosed is a notification for a pull request that was closed without being merged.
	CategoryClosed Category = "closed"
	// CategoryCIFailure is a notification for a failed CI run.
	CategoryCIFailure Category = "ci failure"
)

// API issues requests to the GitHub REST API. It is satisfied by the go-gh REST client.
type API interface {
	DoWithContext(ctx context.Context, method, path string, body io.Reader, response any) error
}

// Options configure a Cleaner.
type Options struct {
	// Merged selects notifications for merged pull requests.
	Merged bool
	// Closed selects notifications for pull requests that were closed without being merged.
	Closed bool
	// CIFailure selects notifications for failed CI runs.
	CIFailure bool
	// DryRun reports what would be marked as done without changing anything.
	DryRun bool
}

// Cleaner marks notifications as done.
type Cleaner struct {
	api         API
	logger      *slog.Logger
	out         io.Writer
	opts        Options
	pageSize    int
	concurrency int
}

// New returns a Cleaner that writes one line per cleaned up notification to out.
func New(api API, logger *slog.Logger, out io.Writer, opts Options) *Cleaner {
	return &Cleaner{
		api:         api,
		logger:      logger,
		out:         out,
		opts:        opts,
		pageSize:    pageSize,
		concurrency: defaultConcurrency,
	}
}

// Summary counts the notifications seen by a run.
type Summary struct {
	// Inspected is the number of notifications in the inbox.
	Inspected int
	// Cleaned is the number of notifications marked as done, or that would be in a dry run.
	Cleaned int
}

type notification struct {
	ID      string `json:"id"`
	Reason  string `json:"reason"`
	Subject struct {
		Title string `json:"title"`
		URL   string `json:"url"`
		Type  string `json:"type"`
	} `json:"subject"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

type pullRequest struct {
	State  string `json:"state"`
	Merged bool   `json:"merged"`
}

type result struct {
	category Category
	selected bool
	err      error
}

// Run marks every selected notification in the inbox as done. A notification that fails to be processed is logged
// and reported in the returned error after all other notifications have been handled.
func (c *Cleaner) Run(ctx context.Context) (Summary, error) {
	notifications, err := c.list(ctx)
	if err != nil {
		return Summary{}, fmt.Errorf("list notifications: %w", err)
	}
	results := make([]result, len(notifications))
	var wg sync.WaitGroup
	sem := make(chan struct{}, c.concurrency)
	for i := range notifications {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = c.process(ctx, notifications[i])
		}()
	}
	wg.Wait()
	summary := Summary{Inspected: len(notifications)}
	var errs []error
	for i, res := range results {
		n := notifications[i]
		switch {
		case res.err != nil:
			c.logger.Warn("skipping notification", "repo", n.Repository.FullName, "title", n.Subject.Title, "error", res.err)
			errs = append(errs, res.err)
		case res.selected:
			summary.Cleaned++
			c.report(n, res.category)
		}
	}
	return summary, errors.Join(errs...)
}

// process classifies a notification and, unless this is a dry run, marks it as done when it is selected.
func (c *Cleaner) process(ctx context.Context, n notification) result {
	category, selected, err := c.classify(ctx, n)
	if err != nil {
		return result{err: fmt.Errorf("classify %s %q: %w", n.Repository.FullName, n.Subject.Title, err)}
	}
	if !selected {
		return result{}
	}
	if !c.opts.DryRun {
		if err := c.api.DoWithContext(ctx, http.MethodDelete, "notifications/threads/"+n.ID, nil, nil); err != nil {
			return result{err: fmt.Errorf("mark %s %q as done: %w", n.Repository.FullName, n.Subject.Title, err)}
		}
	}
	return result{category: category, selected: true}
}

func (c *Cleaner) report(n notification, category Category) {
	verb := "done"
	if c.opts.DryRun {
		verb = "would mark done"
	}
	_, _ = fmt.Fprintf(c.out, "%s: %s: %s (%s)\n", verb, n.Repository.FullName, n.Subject.Title, category)
}

// classify reports why a notification should be cleaned up, and whether it is selected by the options.
func (c *Cleaner) classify(ctx context.Context, n notification) (Category, bool, error) {
	switch {
	case n.Subject.Type == subjectPullRequest && (c.opts.Merged || c.opts.Closed):
		return c.classifyPullRequest(ctx, n)
	case isCIFailure(n):
		return CategoryCIFailure, c.opts.CIFailure, nil
	default:
		return "", false, nil
	}
}

func (c *Cleaner) classifyPullRequest(ctx context.Context, n notification) (Category, bool, error) {
	var pr pullRequest
	if err := c.api.DoWithContext(ctx, http.MethodGet, n.Subject.URL, nil, &pr); err != nil {
		var httpErr *api.HTTPError
		if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
			// The pull request is gone or no longer accessible: leave the notification alone.
			c.logger.Debug("pull request not found", "url", n.Subject.URL)
			return "", false, nil
		}
		return "", false, fmt.Errorf("get pull request: %w", err)
	}
	switch {
	case pr.State != "closed":
		return "", false, nil
	case pr.Merged:
		return CategoryMerged, c.opts.Merged, nil
	default:
		return CategoryClosed, c.opts.Closed, nil
	}
}

// isCIFailure reports whether a notification is about a failed CI run. GitHub titles these notifications like
// "CI workflow run failed for main branch".
func isCIFailure(n notification) bool {
	return n.Reason == reasonCIActivity &&
		n.Subject.Type == subjectCheckSuite &&
		strings.Contains(strings.ToLower(n.Subject.Title), "failed")
}

// list returns every notification in the inbox that is not done, whether read or unread.
func (c *Cleaner) list(ctx context.Context) ([]notification, error) {
	var all []notification
	for page := 1; ; page++ {
		var batch []notification
		path := fmt.Sprintf("notifications?all=true&is_done=false&per_page=%d&page=%d", c.pageSize, page)
		if err := c.api.DoWithContext(ctx, http.MethodGet, path, nil, &batch); err != nil {
			return nil, fmt.Errorf("page %d: %w", page, err)
		}
		all = append(all, batch...)
		if len(batch) < c.pageSize {
			return all, nil
		}
	}
}
