package instance

import (
	"context"
	"reflect"
	"testing"

	"github.com/benjaco/devflow/pkg/api"
)

func TestChoicePromptValidationAndReconnect(t *testing.T) {
	root := t.TempDir()
	const id = "choice-test"
	run := testPromptRunID(t, root, id)
	attempt := testPromptAttemptID(t, root, id, "migration")
	prompt, err := CreatePrompt(context.Background(), root, id, api.Prompt{RunID: run, Task: "migration", AttemptID: attempt, Kind: "select", Message: "Rename or create?", Choices: []string{"create", "rename"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range []int{-1, 2} {
		err := RespondPrompt(context.Background(), root, id, api.PromptAnswer{RunID: run, Task: prompt.Task, AttemptID: attempt, PromptID: prompt.ID, Choice: &index})
		assertPromptError(t, err, "invalid_prompt_answer")
	}
	yes := true
	first := 0
	for _, answer := range []api.PromptAnswer{{Confirm: &yes}, {Confirm: &yes, Choice: &first}, {Choice: &first, Cancel: true}, {}} {
		answer.RunID, answer.Task, answer.AttemptID, answer.PromptID = run, prompt.Task, attempt, prompt.ID
		assertPromptError(t, RespondPrompt(context.Background(), root, id, answer), "invalid_prompt_answer")
	}
	listed, err := ListPrompts(context.Background(), root, id, run)
	if err != nil || len(listed) != 1 || !reflect.DeepEqual(listed[0].Choices, prompt.Choices) {
		t.Fatalf("reconnect lost options: %+v, %v", listed, err)
	}
	answer := api.PromptAnswer{RunID: run, Task: prompt.Task, AttemptID: attempt, PromptID: prompt.ID, Choice: &first}
	if err := RespondPrompt(context.Background(), root, id, answer); err != nil {
		t.Fatal(err)
	}
	got, err := ConsumePromptAnswer(context.Background(), root, id, run, prompt.ID)
	if err != nil || got == nil || got.Choice == nil || *got.Choice != 0 {
		t.Fatalf("zero choice lost: %+v, %v", got, err)
	}
	assertPromptError(t, RespondPrompt(context.Background(), root, id, answer), "prompt_not_pending")
}
