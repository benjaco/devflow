package ports

import "testing"

func TestAllocateUniquePorts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	manager, err := NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	first, err := manager.Allocate("one", []string{"backend", "frontend"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Allocate("two", []string{"backend", "frontend"})
	if err != nil {
		t.Fatal(err)
	}
	if first["backend"] == second["backend"] || first["frontend"] == second["frontend"] {
		t.Fatalf("ports collided: first=%v second=%v", first, second)
	}
}
