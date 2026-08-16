package validation

import (
	"strings"
	"unicode/utf8"

	"github.com/benjaco/devflow/pkg/api"
)

type validationSampler struct {
	maxPerCategory int
	maxBytes       int64
	bytes          int64
	counts         map[string]int
	seen           map[string]int
}

func applyValidationDetails(result *api.ValidationResult, req Request) {
	if result == nil {
		return
	}
	result.IssueCount = len(result.Issues)
	sampler := &validationSampler{
		maxPerCategory: req.MaxListedPaths,
		maxBytes:       req.MaxListedBytes,
		counts:         map[string]int{},
		seen:           map[string]int{},
	}

	if result.Artifacts != nil {
		artifacts := result.Artifacts
		artifacts.IssueCount = len(artifacts.Issues)
		artifacts.Samples = emptyValidationSamples()
		for index := range artifacts.Tasks {
			task := &artifacts.Tasks[index]
			task.MaterializedInputCount = len(task.MaterializedInputs)
			task.DependencyOutputCount = len(task.DependencyOutputs)
			task.ProducedPathCount = len(task.ProducedOutputs)
			task.ObservedWriteCount = len(task.ObservedWrites)
			task.UndeclaredWriteCount = len(task.UndeclaredWrites)
			task.MissingOutputCount = len(task.MissingOutputs)
			task.IssueCount = len(task.Issues)
			task.Samples = emptyValidationSamples()
			artifacts.ObservedWriteCount += task.ObservedWriteCount
			artifacts.ProducedPathCount += task.ProducedPathCount
			artifacts.UndeclaredWriteCount += task.UndeclaredWriteCount
			artifacts.MissingOutputCount += task.MissingOutputCount

			if req.Details == api.ValidationDetailsIssues {
				for _, path := range task.UndeclaredWrites {
					if sampled, ok := sampler.take("undeclaredWrites", path); ok {
						artifacts.Samples.UndeclaredWrites = append(artifacts.Samples.UndeclaredWrites, sampled)
					}
				}
				for _, path := range task.MissingOutputs {
					if sampled, ok := sampler.take("missingOutputs", path); ok {
						artifacts.Samples.MissingOutputs = append(artifacts.Samples.MissingOutputs, sampled)
					}
				}
				classifyIssueSamples(task.Issues, sampler, &artifacts.Samples)
			}
		}
		artifacts.Truncated = api.ValidationPathTruncation{
			MaterializedInputs: req.Details != api.ValidationDetailsFull && anyTaskCount(artifacts.Tasks, func(task api.ArtifactTaskValidation) int { return task.MaterializedInputCount }) > 0,
			DependencyOutputs:  req.Details != api.ValidationDetailsFull && anyTaskCount(artifacts.Tasks, func(task api.ArtifactTaskValidation) int { return task.DependencyOutputCount }) > 0,
			ProducedPaths:      req.Details != api.ValidationDetailsFull && artifacts.ProducedPathCount > 0,
			ObservedWrites:     req.Details != api.ValidationDetailsFull && artifacts.ObservedWriteCount > 0,
			UndeclaredWrites:   artifacts.UndeclaredWriteCount > len(artifacts.Samples.UndeclaredWrites),
			MissingOutputs:     artifacts.MissingOutputCount > len(artifacts.Samples.MissingOutputs),
			RejectedSymlinks:   sampler.wasTruncated("rejectedSymlinks", len(artifacts.Samples.RejectedSymlinks)),
			CopyFailures:       sampler.wasTruncated("copyFailures", len(artifacts.Samples.CopyFailures)),
			OtherIssues:        sampler.wasTruncated("otherIssues", len(artifacts.Samples.OtherIssues)),
		}

		if req.Details != api.ValidationDetailsFull {
			for index := range artifacts.Tasks {
				task := &artifacts.Tasks[index]
				task.Truncated = api.ValidationPathTruncation{
					MaterializedInputs: task.MaterializedInputCount > 0,
					DependencyOutputs:  task.DependencyOutputCount > 0,
					ProducedPaths:      task.ProducedPathCount > 0,
					ObservedWrites:     task.ObservedWriteCount > 0,
					UndeclaredWrites:   task.UndeclaredWriteCount > 0,
					MissingOutputs:     task.MissingOutputCount > 0,
				}
				task.MaterializedInputs = nil
				task.DependencyOutputs = nil
				task.ProducedOutputs = nil
				task.ObservedWrites = nil
				task.UndeclaredWrites = nil
				task.MissingOutputs = nil
				task.Issues = nil
				if task.Log != "" {
					if sampled, ok := sampler.take("logs", task.Log); ok {
						task.Log = sampled
					} else {
						task.Log = ""
					}
				}
				if req.Details == api.ValidationDetailsSummary {
					task.DeclaredInputs = nil
					task.DeclaredOutputs = nil
					task.Error = ""
					task.Log = ""
				}
			}
			artifacts.Issues = nil
		}
	}

	if result.Orders != nil && req.Details != api.ValidationDetailsFull {
		for index := range result.Orders.Runs {
			run := &result.Orders.Runs[index]
			run.OutputDifferences = sampler.takeMany("orderOutputDifferences", run.OutputDifferences)
			if run.Log != "" {
				if sampled, ok := sampler.take("logs", run.Log); ok {
					run.Log = sampled
				} else {
					run.Log = ""
				}
			}
			if req.Details == api.ValidationDetailsSummary {
				run.Tasks = nil
				run.Error = ""
				run.Log = ""
				run.OutputDifferences = nil
			}
		}
		result.Orders.Issues = nil
	}

	if req.Details == api.ValidationDetailsFull {
		return
	}
	if req.Details == api.ValidationDetailsSummary {
		result.Issues = nil
		return
	}
	result.Issues = sampleValidationIssues(result.Issues, sampler)
}

func emptyValidationSamples() api.ValidationPathSamples {
	return api.ValidationPathSamples{
		UndeclaredWrites: []string{},
		MissingOutputs:   []string{},
		RejectedSymlinks: []string{},
		CopyFailures:     []string{},
		OtherIssues:      []string{},
	}
}

func classifyIssueSamples(issues []api.ValidationIssue, sampler *validationSampler, samples *api.ValidationPathSamples) {
	for _, issue := range issues {
		value := issue.Path
		if value == "" {
			value = issue.Message
		}
		switch {
		case strings.Contains(issue.Kind, "symlink"):
			if sampled, ok := sampler.take("rejectedSymlinks", value); ok {
				samples.RejectedSymlinks = append(samples.RejectedSymlinks, sampled)
			}
		case strings.Contains(issue.Kind, "copy") || strings.Contains(issue.Kind, "materializ"):
			if sampled, ok := sampler.take("copyFailures", value); ok {
				samples.CopyFailures = append(samples.CopyFailures, sampled)
			}
		case issue.Kind != "undeclared_output" && issue.Kind != "missing_output":
			if sampled, ok := sampler.take("otherIssues", value); ok {
				samples.OtherIssues = append(samples.OtherIssues, sampled)
			}
		}
	}
}

func sampleValidationIssues(issues []api.ValidationIssue, sampler *validationSampler) []api.ValidationIssue {
	out := make([]api.ValidationIssue, 0, min(len(issues), sampler.maxPerCategory))
	for _, issue := range issues {
		category := "issue:" + issue.Kind
		value := issue.Path
		if value == "" {
			value = issue.Message
		}
		sampled, ok := sampler.take(category, value)
		if !ok {
			continue
		}
		if issue.Path != "" {
			issue.Path = sampled
		} else {
			issue.Message = sampled
		}
		if len(issue.Message) > 4096 {
			issue.Message = truncateValidationText(issue.Message, 4096)
		}
		out = append(out, issue)
	}
	return out
}

func (s *validationSampler) takeMany(category string, values []string) []string {
	out := make([]string, 0, min(len(values), s.maxPerCategory))
	for _, value := range values {
		if sampled, ok := s.take(category, value); ok {
			out = append(out, sampled)
		}
	}
	return out
}

func (s *validationSampler) take(category, value string) (string, bool) {
	if s == nil {
		return "", false
	}
	s.seen[category]++
	if s.maxPerCategory <= 0 || s.counts[category] >= s.maxPerCategory || s.bytes >= s.maxBytes {
		return "", false
	}
	remaining := s.maxBytes - s.bytes
	if remaining <= 0 {
		return "", false
	}
	if int64(len(value)+1) > remaining {
		value = truncateValidationText(value, int(remaining-1))
	}
	if value == "" {
		return "", false
	}
	s.counts[category]++
	s.bytes += int64(len(value) + 1)
	return value, true
}

func (s *validationSampler) wasTruncated(category string, listed int) bool {
	return s.seen[category] > listed
}

func truncateValidationText(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	const suffix = "…"
	if len(value) <= maxBytes {
		return value
	}
	if maxBytes <= len(suffix) {
		return strings.Repeat(".", maxBytes)
	}
	value = value[:maxBytes-len(suffix)]
	for value != "" && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value + suffix
}

func anyTaskCount(tasks []api.ArtifactTaskValidation, value func(api.ArtifactTaskValidation) int) int {
	total := 0
	for _, task := range tasks {
		total += value(task)
	}
	return total
}
