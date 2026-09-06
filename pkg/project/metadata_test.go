package project

import (
	"context"
	"reflect"
	"testing"
)

func TestBuilderDeclaresVerificationPurposesAndEffects(t *testing.T) {
	effects := Effects{
		Writes: []string{"generated/**"}, Touches: []string{"database"}, Invalidates: []string{"build"},
		Resources: []ResourceUse{{Name: "database", Access: ResourceRead}},
	}
	p := Define(func(ctx context.Context, b *Builder) error {
		b.Name("metadata")
		check := b.Task("check").Purposes(PurposeTest, PurposeLint).Effects(effects)
		b.Task("read-only").Effects(Effects{})
		b.Task("unknown")
		b.GoDebugService("debug").Purposes(PurposeBuild).Effects(Effects{Resources: []ResourceUse{{Name: "debugger", Access: ResourceWrite}}})
		b.VerificationTarget("verify", check)
		b.Target("up", "unknown")
		return nil
	})
	effects.Writes[0] = "changed"
	effects.Touches[0] = "changed"
	effects.Invalidates[0] = "changed"
	effects.Resources[0].Name = "changed"
	check := taskByNameForTest(p.Tasks(), "check")
	if !reflect.DeepEqual(check.Purposes, []Purpose{PurposeTest, PurposeLint}) {
		t.Fatalf("purposes = %v", check.Purposes)
	}
	if check.Effects.Writes[0] != "generated/**" || check.Effects.Touches[0] != "database" || check.Effects.Invalidates[0] != "build" || check.Effects.Resources[0].Name != "database" {
		t.Fatalf("builder retained mutable effects: %+v", check.Effects)
	}
	check.Purposes[0] = PurposeFormat
	check.Effects.Resources[0].Access = ResourceWrite
	if again := taskByNameForTest(p.Tasks(), "check"); again.Purposes[0] != PurposeTest || again.Effects.Resources[0].Access != ResourceRead {
		t.Fatalf("returned metadata mutated project: %+v", again)
	}
	if taskByNameForTest(p.Tasks(), "read-only").Effects == nil || taskByNameForTest(p.Tasks(), "unknown").Effects != nil {
		t.Fatal("explicit empty effects must remain distinct from unknown effects")
	}
	debug := taskByNameForTest(p.Tasks(), "debug")
	if debug.Purposes[0] != PurposeBuild || debug.Effects.Resources[0].Name != "debugger" {
		t.Fatalf("debug metadata = %+v", debug)
	}
	targets := p.Targets()
	if targets[0].Name != "verify" || !targets[0].Verification || !reflect.DeepEqual(targets[0].RootTasks, []string{"check"}) || targets[1].Verification {
		t.Fatalf("target declarations = %+v", targets)
	}
}

func TestActionEffectsResourcesAreCopied(t *testing.T) {
	p := builtProject{actions: []Action{{ID: "generate", Effects: Effects{Resources: []ResourceUse{{Name: "database", Access: ResourceWrite}}}}}}
	first := Actions(p)
	first[0].Effects.Resources[0].Name = "changed"
	if got := Actions(p)[0].Effects.Resources[0]; got.Name != "database" || got.Access != ResourceWrite {
		t.Fatalf("action effects share mutable resources: %+v", got)
	}
}
