package project

import (
	"fmt"
	"sort"
	"strings"
)

type ActionCategory string

const (
	ActionCategoryAuthoring ActionCategory = "authoring"
	ActionCategoryOperator  ActionCategory = "operator"
	ActionCategoryUtility   ActionCategory = "utility"
)

type ActionRelaunchPolicy string

const (
	ActionRelaunchNever                      ActionRelaunchPolicy = "never"
	ActionRelaunchPreviousTargetAfterSuccess ActionRelaunchPolicy = "previous_target_after_success"
)

type ActionInputType string

const (
	ActionInputString ActionInputType = "string"
	ActionInputBool   ActionInputType = "bool"
)

type ActionInput struct {
	Name        string          `json:"name"`
	Type        ActionInputType `json:"type"`
	Label       string          `json:"label,omitempty"`
	Description string          `json:"description,omitempty"`
	Required    bool            `json:"required,omitempty"`
	Positional  bool            `json:"positional,omitempty"`
	Env         string          `json:"env,omitempty"`
	Default     string          `json:"default,omitempty"`
}

type Action struct {
	ID          string               `json:"id"`
	Kind        string               `json:"kind,omitempty"`
	Category    ActionCategory       `json:"category,omitempty"`
	Label       string               `json:"label,omitempty"`
	Description string               `json:"description,omitempty"`
	Component   string               `json:"component,omitempty"`
	Task        string               `json:"task,omitempty"`
	Inputs      []ActionInput        `json:"inputs,omitempty"`
	Effects     Effects              `json:"effects,omitempty"`
	Relaunch    ActionRelaunchPolicy `json:"relaunch,omitempty"`
	Aliases     []string             `json:"aliases,omitempty"`
}

type ActionProvider interface {
	Actions() []Action
}

func Actions(p Project) []Action {
	if p == nil {
		return nil
	}
	provider, ok := p.(ActionProvider)
	if !ok {
		return nil
	}
	actions := provider.Actions()
	out := make([]Action, 0, len(actions))
	for _, action := range actions {
		action.Aliases = uniqueStrings(action.Aliases)
		action.Inputs = cloneActionInputs(action.Inputs)
		action.Effects = cloneEffects(action.Effects)
		out = append(out, action)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func ResolveAction(p Project, idOrAlias, kind, component string) (Action, error) {
	idOrAlias = strings.TrimSpace(idOrAlias)
	kind = strings.TrimSpace(kind)
	component = strings.TrimSpace(component)
	actions := Actions(p)
	matches := make([]Action, 0, 1)
	for _, action := range actions {
		if idOrAlias != "" && !actionMatchesIDOrAlias(action, idOrAlias) {
			continue
		}
		if kind != "" && action.Kind != kind {
			continue
		}
		if component != "" && action.Component != component {
			continue
		}
		matches = append(matches, action)
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		if idOrAlias != "" {
			return Action{}, fmt.Errorf("unknown action %q", idOrAlias)
		}
		if kind != "" {
			if component != "" {
				return Action{}, fmt.Errorf("no action of kind %q for component %q", kind, component)
			}
			return Action{}, fmt.Errorf("no action of kind %q", kind)
		}
		return Action{}, fmt.Errorf("no action selected")
	default:
		names := make([]string, 0, len(matches))
		for _, action := range matches {
			names = append(names, action.ID)
		}
		sort.Strings(names)
		return Action{}, fmt.Errorf("ambiguous action; candidates: %s", strings.Join(names, ", "))
	}
}

func actionMatchesIDOrAlias(action Action, idOrAlias string) bool {
	if action.ID == idOrAlias {
		return true
	}
	for _, alias := range action.Aliases {
		if alias == idOrAlias {
			return true
		}
	}
	return false
}

func cloneActionInputs(in []ActionInput) []ActionInput {
	if len(in) == 0 {
		return nil
	}
	return append([]ActionInput(nil), in...)
}
