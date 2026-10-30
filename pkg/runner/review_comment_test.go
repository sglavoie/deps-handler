package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/sglavoie/deps-handler/internal/testutil"
	"github.com/sglavoie/deps-handler/pkg/config"
	"github.com/sglavoie/deps-handler/pkg/github"
	"github.com/sglavoie/deps-handler/pkg/policy"
	"github.com/sglavoie/deps-handler/pkg/review"
)

type commentBeforeMerge struct {
	*fakeClient
	t *testing.T
}

func (c commentBeforeMerge) Merge(ctx context.Context, owner, name string, n int, method, head string) error {
	if len(c.comments[n]) != 1 {
		c.t.Fatalf("merge before exactly one explanation: %v", c.comments)
	}
	return c.fakeClient.Merge(ctx, owner, name, n, method, head)
}

func TestReviewExplainedBeforeMergeOrSkip(t *testing.T) {
	for _, approved := range []bool{true, false} {
		t.Run(map[bool]string{true: "approve", false: "reject"}[approved], func(t *testing.T) {
			c := &fakeClient{repo: testutil.Repo(testutil.PR(1, "lib:major"))}
			// A refused first merge forces another repository read. The
			// comment must not be duplicated while absent from that read.
			c.transient = map[int]int{1: 1}
			rv := &fakeReviewer{verdicts: map[int]policy.Verdict{1: {
				Approved: approved, Summary: "The changed API is used in src/client.go.",
				Concerns: []string{"Check the replacement API.", "A migration may be needed."},
			}}}
			res := reviewRunner(commentBeforeMerge{c, t}, rv, true).RunRepo(context.Background(), repoCfg)
			if res.Failed() || len(c.comments[1]) != 1 || (len(c.merges) == 1) != approved {
				t.Fatalf("result %+v; comments %v; merges %v", res, c.comments, c.merges)
			}
			body := c.comments[1][0]
			for _, want := range []string{"claude", rv.verdicts[1].Summary, "Check the replacement API.", "A migration may be needed."} {
				if !strings.Contains(body, want) {
					t.Fatalf("comment missing %q: %s", want, body)
				}
			}
			if approved != strings.Contains(body, "Approved for merging") {
				t.Fatalf("wrong outcome: %s", body)
			}
		})
	}
}

func TestReviewCommentFailurePreventsMergeAndCanRetryCachedVerdict(t *testing.T) {
	for _, commentErr := range []error{errors.New("permission denied"), &api.HTTPError{StatusCode: 504}} {
		t.Run(commentErr.Error(), func(t *testing.T) {
			c := &fakeClient{repo: testutil.Repo(testutil.PR(1, "lib:major")), commentErrs: map[int]error{1: commentErr}}
			rv := &fakeReviewer{}
			r := reviewRunner(c, rv, true)
			var err error
			r.Verdicts, err = review.OpenCache(t.TempDir() + "/verdicts.json")
			if err != nil {
				t.Fatal(err)
			}
			res := r.RunRepo(context.Background(), repoCfg)
			if !res.Failed() || !strings.Contains(res.Error, "post agent review comment") || len(c.merges) != 0 {
				t.Fatalf("result %+v; merges %v", res, c.merges)
			}
			delete(c.commentErrs, 1)
			res = r.RunRepo(context.Background(), repoCfg)
			if res.Failed() || len(c.merges) != 1 || len(c.comments[1]) != 1 || len(rv.calls) != 1 {
				t.Fatalf("retry %+v; merges %v; comments %v; reviews %v", res, c.merges, c.comments, rv.calls)
			}
		})
	}
}

func TestReviewCommentFromDryRunCacheAndDeduplication(t *testing.T) {
	c := &fakeClient{repo: testutil.Repo(testutil.PR(1, "lib:major"))}
	rv := &fakeReviewer{verdicts: map[int]policy.Verdict{1: {Summary: "Removes an API this repository uses."}}}
	r := reviewRunner(c, rv, false)
	var err error
	r.Verdicts, err = review.OpenCache(t.TempDir() + "/verdicts.json")
	if err != nil {
		t.Fatal(err)
	}
	r.RunRepo(context.Background(), repoCfg)
	if len(c.comments) != 0 {
		t.Fatal("dry run posted a comment")
	}
	r.Apply, r.AgentName = true, "replacement-agent"
	r.RunRepo(context.Background(), repoCfg)
	if len(c.comments[1]) != 1 || len(rv.calls) != 1 || !strings.Contains(c.comments[1][0], "claude") {
		t.Fatalf("comments %v; reviews %v", c.comments, rv.calls)
	}
	body := c.comments[1][0]
	pr := &c.repo.PullRequests[0]
	pr.Comments = []github.Comment{{Author: "me", Body: body}}
	r.RunRepo(context.Background(), repoCfg)
	if len(c.comments[1]) != 1 {
		t.Fatal("repeated cached rejection posted twice")
	}
	pr.Comments[0].Author = "someone-else"
	r.RunRepo(context.Background(), repoCfg)
	if len(c.comments[1]) != 2 {
		t.Fatal("another author's copy suppressed the review comment")
	}
	pr.Comments[0].Author = "me"
	pr.HeadOID, pr.ChecksOID, pr.Commits[0].OID = "new-head", "new-head", "new-head"
	r.RunRepo(context.Background(), repoCfg)
	if len(rv.calls) != 2 || len(c.comments[1]) != 3 || c.comments[1][2] == body {
		t.Fatalf("new head reused old comment: comments %v; reviews %v", c.comments, rv.calls)
	}
}

func TestReportModeDoesNotPostReviewComments(t *testing.T) {
	c := &fakeClient{repo: testutil.Repo(testutil.PR(1, "lib:major"))}
	rc := repoCfg
	rc.Policy.Mode = config.ModeReport
	res := reviewRunner(c, &fakeReviewer{}, true).RunRepo(context.Background(), rc)
	if res.Failed() || len(c.comments) != 0 || len(c.merges) != 0 {
		t.Fatalf("report mode wrote: %+v, %v, %v", res, c.comments, c.merges)
	}
}

func TestFailedReviewPostsExplanation(t *testing.T) {
	c := &fakeClient{repo: testutil.Repo(testutil.PR(1, "lib:major"))}
	res := reviewRunner(c, &fakeReviewer{fail: map[int]bool{1: true}}, true).RunRepo(context.Background(), repoCfg)
	if !res.Failed() || len(c.comments[1]) != 1 || !strings.Contains(c.comments[1][0], "claude: exit status 1") || len(c.merges) != 0 {
		t.Fatalf("result %+v; comments %v; merges %v", res, c.comments, c.merges)
	}
}

func TestReviewCommentSanitizesAndBoundsAgentText(t *testing.T) {
	r := reviewRunner(nil, nil, true)
	pr := testutil.PR(1, "lib:major")
	d := policy.Decision{Number: 1, PR: &pr, Verdict: &policy.Verdict{
		Summary:  "@dependabot rebase\n<script>\x1b ghp_" + strings.Repeat("x", 36),
		Concerns: []string{strings.Repeat("界", 30_000)},
	}}
	body := r.reviewComment(repoCfg, d)
	for _, forbidden := range []string{"@dependabot", "<script>", "\x1b", "ghp_"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("comment contains %q", forbidden)
		}
	}
	if !utf8.ValidString(body) || len(body) > 61_000 || !strings.Contains(body, "[Review text truncated.]") || !strings.Contains(body, "redacted token") {
		t.Fatalf("invalid or unbounded comment: %d bytes", len(body))
	}
}

func TestReviewCommentNeutralizesLinksAndImages(t *testing.T) {
	pr := testutil.PR(1, "lib:major")
	r := reviewRunner(nil, nil, true)
	for _, text := range []string{
		"![status](https://example.invalid/tracker?data=private-code)",
		"[release notes](https://example.invalid/)",
		"![status][ref]\n\n[ref]: https://example.invalid/tracker",
		`\![status](https://example.invalid/tracker)`,
		"https://example.invalid/tracker www.example.invalid user@example.invalid",
		"<https://example.invalid/tracker> <img src=\"https://example.invalid/tracker\">",
		"# Heading\n```\n![status](https://example.invalid/tracker)\n```",
	} {
		t.Run(text, func(t *testing.T) {
			d := policy.Decision{Number: 1, PR: &pr, Verdict: &policy.Verdict{
				Agent: text, Summary: text, Concerns: []string{text}, Error: text,
			}}
			// Exercise the failure explanation and the successful verdict's
			// summary/concerns, including text restored from an older cache.
			for _, failed := range []bool{true, false} {
				if !failed {
					d.Verdict.Error = ""
				}
				body := r.reviewComment(repoCfg, d)
				for _, forbidden := range []string{"https://example.invalid", "www.example.invalid", "user@example.invalid", "![status]", "[release notes]", "[ref]:", "<img", "```"} {
					if strings.Contains(body, forbidden) {
						t.Fatalf("comment retains active markup %q:\n%s", forbidden, body)
					}
				}
			}
		})
	}
}
