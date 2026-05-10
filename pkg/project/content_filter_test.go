package project

import (
	"context"
	"strings"
	"testing"
)

func TestGoStructDeclarationsIncludesDocComments(t *testing.T) {
	filter := GoStructDeclarations()
	content := []byte(`package demo

// User is returned by the API.
// @name UserPayload
type User struct {
	ID int ` + "`json:\"id\"`" + `
}

func handler() {}

type Alias string
`)

	filtered, err := filter.Apply(context.Background(), nil, FileContent{Path: "api.go", Content: content})
	if err != nil {
		t.Fatal(err)
	}
	got := string(filtered)
	for _, want := range []string{"// User is returned by the API.", "// @name UserPayload", "type User struct"} {
		if !strings.Contains(got, want) {
			t.Fatalf("filtered content missing %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"handler", "type Alias"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("filtered content unexpectedly contains %q:\n%s", unwanted, got)
		}
	}
}

func TestCombineContentFiltersForGoCommentsAndStructs(t *testing.T) {
	filter := CombineContentFilters(GoCommentLinesStartingWith("@"), GoStructDeclarations())
	content := []byte(`package demo

// @Summary List users
// plain handler comment
func listUsers() {}

// User is returned by the API.
type User struct {
	Email string
}
`)

	filtered, err := filter.Apply(context.Background(), nil, FileContent{Path: "api.go", Content: content})
	if err != nil {
		t.Fatal(err)
	}
	got := string(filtered)
	if !strings.Contains(got, "@Summary List users") {
		t.Fatalf("filtered content missing swaggo annotation:\n%s", got)
	}
	if !strings.Contains(got, "// User is returned by the API.") || !strings.Contains(got, "type User struct") {
		t.Fatalf("filtered content missing struct declaration with doc:\n%s", got)
	}
	if strings.Contains(got, "plain handler comment") {
		t.Fatalf("filtered content should not include unrelated comments:\n%s", got)
	}
}
