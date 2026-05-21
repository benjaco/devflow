package project

import (
	"context"
	"testing"
)

func TestBuilderDefinesAndResolvesActions(t *testing.T) {
	p := Define(func(ctx context.Context, b *Builder) error {
		b.Name("actions-demo")
		task := b.Task("make_thing").NoCache()
		b.Target("up", task)
		b.Action("thing.create").
			Kind("devflow.test.create").
			Category(ActionCategoryAuthoring).
			Label("Create thing").
			Component("thing").
			Task(task).
			Input(ActionInput{
				Name:       "name",
				Type:       ActionInputString,
				Required:   true,
				Positional: true,
				Env:        "THING_NAME",
			}).
			Writes("things/**").
			Invalidates(task).
			RelaunchPreviousTargetAfterSuccess().
			Alias("thing:create")
		return nil
	})

	actions := Actions(p)
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	action := actions[0]
	if action.ID != "thing.create" || action.Kind != "devflow.test.create" || action.Component != "thing" {
		t.Fatalf("unexpected action: %+v", action)
	}
	if action.Task != "make_thing" {
		t.Fatalf("unexpected action task %q", action.Task)
	}
	if action.Relaunch != ActionRelaunchPreviousTargetAfterSuccess {
		t.Fatalf("unexpected relaunch policy %q", action.Relaunch)
	}
	if len(action.Inputs) != 1 || action.Inputs[0].Env != "THING_NAME" {
		t.Fatalf("unexpected inputs: %+v", action.Inputs)
	}

	resolved, err := ResolveAction(p, "thing:create", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ID != "thing.create" {
		t.Fatalf("unexpected resolved action %q", resolved.ID)
	}
}

func TestResolveActionReportsAmbiguousKind(t *testing.T) {
	p := Define(func(ctx context.Context, b *Builder) error {
		b.Name("ambiguous-actions")
		task := b.Task("noop").NoCache()
		b.Target("up", task)
		b.Action("one.create").Kind("devflow.test.create").Component("one").Task(task)
		b.Action("two.create").Kind("devflow.test.create").Component("two").Task(task)
		return nil
	})

	if _, err := ResolveAction(p, "", "devflow.test.create", ""); err == nil {
		t.Fatal("expected ambiguous action error")
	}
	action, err := ResolveAction(p, "", "devflow.test.create", "two")
	if err != nil {
		t.Fatal(err)
	}
	if action.ID != "two.create" {
		t.Fatalf("unexpected resolved action %q", action.ID)
	}
}
