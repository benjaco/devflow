package project

import "slices"

type ResourceAccess string

const (
	ResourceRead  ResourceAccess = "read"
	ResourceWrite ResourceAccess = "write"
)

type ResourceUse struct {
	Name   string         `json:"name"`
	Access ResourceAccess `json:"access"`
}

// Effects describe declared behavior for inspection and planning. They do not
// grant permission or prove that a callback has no other effects.
type Effects struct {
	Writes      []string      `json:"writes,omitempty"`
	Touches     []string      `json:"touches,omitempty"`
	Invalidates []string      `json:"invalidates,omitempty"`
	Resources   []ResourceUse `json:"resources,omitempty"`
}

func cloneEffects(in Effects) Effects {
	return Effects{
		Writes: slices.Clone(in.Writes), Touches: slices.Clone(in.Touches),
		Invalidates: slices.Clone(in.Invalidates), Resources: slices.Clone(in.Resources),
	}
}

func cloneTaskEffects(in *Effects) *Effects {
	if in == nil {
		return nil
	}
	out := cloneEffects(*in)
	return &out
}
