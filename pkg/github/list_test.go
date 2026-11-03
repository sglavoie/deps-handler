package github

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func listRepoPage(root, nodes string, next bool, cursor string) string {
	return fmt.Sprintf(`{"data":{%q:{"repositories":{"nodes":[%s],"pageInfo":{"hasNextPage":%t,"endCursor":%q}}}}}`, root, nodes, next, cursor)
}

func listPRPage(nodes string, next bool, cursor string) string {
	return fmt.Sprintf(`{"data":{"repository":{"pullRequests":{"nodes":[%s],"pageInfo":{"hasNextPage":%t,"endCursor":%q}}}}}`, nodes, next, cursor)
}

func TestListRepositoriesPagesAndOwner(t *testing.T) {
	c, rt := handleAPIClient(t,
		listRepoPage("viewer", `{"nameWithOwner":"o/z"}`, true, "next"),
		listRepoPage("viewer", `{"nameWithOwner":"o/a"},{"nameWithOwner":"O/Z"}`, false, ""))
	got, err := c.ListRepositories(context.Background(), "")
	if err != nil || fmt.Sprint(got) != "[o/a o/z]" || rt.variables[1]["after"] != "next" || !strings.Contains(rt.queries[0], "ownerAffiliations") {
		t.Fatal(got, err, rt)
	}
	c, rt = handleAPIClient(t, listRepoPage("repositoryOwner", `{"nameWithOwner":"other/collaboration"},{"nameWithOwner":"O/a"}`, false, ""))
	got, err = c.ListRepositories(context.Background(), "o")
	if err != nil || fmt.Sprint(got) != "[O/a]" || rt.variables[0]["owner"] != "o" {
		t.Fatal(got, err, rt)
	}
}

func TestListRepositoriesRejectsIncompletePages(t *testing.T) {
	for _, pages := range [][]string{
		{`{"data":{"viewer":null}}`},
		{listRepoPage("viewer", `{"nameWithOwner":"o/a"}`, true, "")},
		{listRepoPage("viewer", `{"nameWithOwner":"o/a"}`, true, "next"), listRepoPage("viewer", `{"nameWithOwner":"bad"}`, false, "")},
		{listRepoPage("viewer", `{"nameWithOwner":"o/a"}`, true, "next"), `{"errors":[{"message":"broken"}]}`},
	} {
		c, _ := handleAPIClient(t, pages...)
		if got, err := c.ListRepositories(context.Background(), ""); err == nil || got != nil {
			t.Fatal(got, err)
		}
	}
}

const listedNode = `{"number":2,"title":"Fix it","url":"https://github.com/o/r/pull/2","isDraft":true,"createdAt":"2026-11-01T10:00:00Z","author":{"login":"dev"}}`

func TestListOpenPRsPagesAndFields(t *testing.T) {
	second := strings.ReplaceAll(listedNode, `"number":2`, `"number":3`)
	second = strings.ReplaceAll(second, `"author":{"login":"dev"}`, `"author":null`)
	c, rt := handleAPIClient(t, listPRPage(listedNode, true, "next"), listPRPage(second, false, ""))
	got, err := c.ListOpenPRs(context.Background(), "o", "r")
	if err != nil || len(got) != 2 || got[0].Repository != "o/r" || got[0].Author != "dev" || !got[0].Draft || got[1].Author != "(deleted)" || rt.variables[1]["after"] != "next" {
		t.Fatal(got, err, rt)
	}
}

func TestListOpenPRsRejectsPartialPages(t *testing.T) {
	for _, pages := range [][]string{
		{`{"data":{"repository":null}}`},
		{listPRPage(strings.ReplaceAll(listedNode, `"isDraft":true,`, ""), false, "")},
		{listPRPage(listedNode, true, "")},
		{listPRPage(listedNode, true, "next"), listPRPage(listedNode, false, "")},
		{listPRPage(listedNode, true, "next"), `{"errors":[{"message":"broken"}]}`},
	} {
		c, _ := handleAPIClient(t, pages...)
		if got, err := c.ListOpenPRs(context.Background(), "o", "r"); err == nil || got != nil {
			t.Fatal(got, err)
		}
	}
}
