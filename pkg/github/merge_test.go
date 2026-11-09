package github

import (
	"context"
	"fmt"
	"testing"
)

func TestMergeMethods(t *testing.T) {
	for _, tc := range []struct {
		body, want string
		fail       bool
	}{
		{`{"squashMergeAllowed":true,"mergeCommitAllowed":true,"rebaseMergeAllowed":true}`, "[squash merge rebase]", false},
		{`{"squashMergeAllowed":false,"mergeCommitAllowed":true,"rebaseMergeAllowed":true}`, "[merge rebase]", false},
		{`{"squashMergeAllowed":false,"mergeCommitAllowed":false,"rebaseMergeAllowed":false}`, "[]", false},
		{`null`, "", true},
		{`{"squashMergeAllowed":true,"mergeCommitAllowed":true}`, "", true},
	} {
		c, rt := handleAPIClient(t, `{"data":{"repository":`+tc.body+`}}`)
		methods, err := c.MergeMethods(context.Background(), "someone", "project")
		if (err != nil) != tc.fail || (!tc.fail && fmt.Sprint(methods) != tc.want) {
			t.Fatal(methods, err)
		}
		if rt.variables[0]["owner"] != "someone" || rt.variables[0]["name"] != "project" {
			t.Fatal(rt.variables)
		}
	}
}
