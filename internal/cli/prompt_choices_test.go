package cli

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/instance"
)

func TestCLIChoicePromptContract(t *testing.T) {
	for _, option := range []string{"choice", "cancel"} {
		t.Run(option, func(t *testing.T) {
			root, record := retainedCLIRun(t, api.RunRunning)
			attempt := instance.NewAttemptID()
			record.Attempts = []api.TaskAttempt{{Task: "migration", AttemptID: attempt, State: api.StateRunning}}
			if err := instance.SaveRun(root, record.InstanceID, record); err != nil {
				t.Fatal(err)
			}
			prompt, err := instance.CreatePrompt(context.Background(), root, record.InstanceID, api.Prompt{RunID: record.RunID, Task: "migration", AttemptID: attempt, Kind: "select", Message: "Create or rename?", Choices: []string{"Create column", "Rename existing column"}})
			if err != nil {
				t.Fatal(err)
			}
			result, _, err := runEvidenceCLI(t, "", "prompts", "list", "--worktree", root, "--run", record.RunID, "--json")
			if err != nil {
				t.Fatal(err)
			}
			var listed []api.Prompt
			if err := json.Unmarshal(result["prompts"], &listed); err != nil || len(listed) != 1 || len(listed[0].Choices) != 2 {
				t.Fatalf("choice metadata: %+v %v", listed, err)
			}
			args := []string{"prompts", "respond", prompt.ID, "--worktree", root, "--run", record.RunID, "--task", prompt.Task, "--attempt", attempt, "--json"}
			if option == "choice" {
				for _, bad := range [][]string{{"--choice", "-1"}, {"--choice", "2"}, {"--choice", "0", "--confirm", "true"}} {
					if _, _, err := runEvidenceCLI(t, "", append(args, bad...)...); err == nil {
						t.Fatalf("accepted invalid response: %v", bad)
					}
				}
				args = append(args, "--choice", "0")
			} else {
				args = append(args, "--cancel")
			}
			result, _, err = runEvidenceCLI(t, "", args...)
			if err != nil || string(result["accepted"]) != "true" {
				t.Fatalf("response: %v %v", result, err)
			}
			if _, ok := result["choice"]; ok {
				t.Fatal("acknowledgment echoed the answer")
			}
			answer, err := instance.ConsumePromptAnswer(context.Background(), root, record.InstanceID, record.RunID, prompt.ID)
			if err != nil || answer == nil {
				t.Fatalf("missing response: %+v %v", answer, err)
			}
			if option == "choice" && (answer.Choice == nil || *answer.Choice != 0) {
				t.Fatalf("zero choice lost: %+v", answer)
			}
			if option == "cancel" && !answer.Cancel {
				t.Fatalf("cancel lost: %+v", answer)
			}
		})
	}
}
