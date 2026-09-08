package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/benjaco/devflow/pkg/project"
)

type demoProject struct{ project.Project }

func init() { project.Register(demoProject{Project: buildDemoProject()}) }

func (demoProject) DefaultTarget() string { return "verify" }
func (demoProject) DetectWorktree(root string) bool {
	_, err := os.Stat(filepath.Join(root, "devflow.project.go"))
	return err == nil
}

func buildDemoProject() project.Project {
	return project.Define(func(_ context.Context, b *project.Builder) error {
		b.Name("github-actions-demo").DefaultTarget("verify")
		shared := b.Task("shared:generate").
			Inputs("devflow.project.go").
			OutputFiles("out/shared.txt").
			Run(func(_ context.Context, rt *project.Runtime) error {
				rt.EmitLogLine("stdout", "shared artifact generated once for both checks")
				return project.WriteFile(rt, "out/shared.txt", []byte("shared fixture data\n"), 0o600)
			})
		backend := b.Task("backend:build").DependsOn(shared).Run(demoCheck("backend-build")).NoCache()
		frontend := b.Task("frontend:lint").DependsOn(shared).Run(demoCheck("frontend-lint")).NoCache()
		all := b.Group("all", backend, frontend)
		failure := b.Task("intentional:failure").DependsOn(shared).NoCache().
			Run(func(_ context.Context, rt *project.Runtime) error {
				rt.EmitLogLine("stderr", "retained diagnostic from the intentional failure demo")
				return fmt.Errorf("intentional demo failure")
			})
		b.Target("verify", all)
		b.Target("failure", failure)
		return nil
	})
}

func demoCheck(label string) project.RunFunc {
	return func(ctx context.Context, rt *project.Runtime) error {
		if _, err := os.ReadFile(rt.Abs("out/shared.txt")); err != nil {
			return err
		}
		// A few paced lines make simultaneous lifecycle progress easy to observe.
		for line := 1; line <= 6; line++ {
			rt.EmitLogLine("stdout", fmt.Sprintf("%s-line-%d", label, line))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(40 * time.Millisecond):
			}
		}
		return nil
	}
}
