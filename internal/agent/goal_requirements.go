package agent

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// GoalRequirement is one explicitly observable obligation extracted from a
// user goal. SourceText keeps the requirement auditable without retaining any
// model reasoning.
type GoalRequirement struct {
	ID                string
	Description       string
	Type              GoalNodeType
	Required          bool
	EvidencePredicate string
	SourceText        string
	Ordinal           int
}

var requirementArtifactPattern = regexp.MustCompile(`(?i)(?:[a-z0-9_.-]+\.(?:md|txt|json|yaml|yml|go|html)|report\.md|trace\.md)`)

// ExtractExplicitRequirements performs deliberately narrow extraction. It
// recognizes explicit action phrases, test commands, and named artifacts; it
// does not infer unstated work or semantic equivalence between commands.
func ExtractExplicitRequirements(goal string) []GoalRequirement {
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return nil
	}
	clauses := splitRequirementClauses(goal)
	var out []GoalRequirement
	for _, clause := range clauses {
		text := strings.TrimSpace(clause)
		low := strings.ToLower(text)
		if text == "" {
			continue
		}
		if fullGoTestMentioned(low) {
			out = append(out, requirement("run-go-test-all", "run go test ./...", GoalNodeVerification, "go test ./... passed", text))
		}
		if relevantTestMentioned(low) {
			out = append(out, requirement("run-relevant-test", "run the relevant test", GoalNodeVerification, "matching relevant test passed", text))
		}
		// Test creation is an independent explicit obligation even when it is
		// stated in the same clause as implementation work.
		if testCreationMentioned(low) {
			out = append(out, requirement("add-tests", text, GoalNodeTestCreation, "test-file mutation observed", text))
		}
		if artifactMentioned(text, low) {
			out = append(out, requirement("create-artifact", text, GoalNodeArtifact, "named artifact exists with requested content", text))
		}
		if explorationMentioned(low) && len(commaInspectionRequirements(text)) == 0 {
			out = append(out, requirement("explore", text, GoalNodeExploration, "requested repository observation recorded", text))
		}
		implementationAction := strings.Contains(low, "fix ") || strings.Contains(low, "implement ") || strings.Contains(low, "change ") || strings.Contains(low, "refactor ") || (implementationMentioned(low) && !artifactMentioned(text, low))
		if implementationAction {
			out = append(out, requirement("implement", text, GoalNodeImplementation, "requested implementation evidence recorded", text))
		}
		if genericVerificationMentioned(low) && !fullGoTestMentioned(low) && !relevantTestMentioned(low) {
			out = append(out, requirement("verify", text, GoalNodeVerification, "requested verification passed", text))
		}
	}
	// A colon followed by bullet items is a common way to enumerate separate
	// read-only evidence obligations. Keep each item authoritative and
	// independent instead of collapsing the whole list into one observation.
	for _, item := range bulletInspectionRequirements(goal) {
		out = append(out, item)
	}
	for _, item := range commaInspectionRequirements(goal) {
		out = append(out, item)
	}
	return stabilizeRequirementIDs(out)
}

func bulletInspectionRequirements(goal string) []GoalRequirement {
	low := strings.ToLower(goal)
	if !strings.Contains(low, "inspect:") && !strings.Contains(low, "collect:") && !strings.Contains(low, "investigate:") {
		return nil
	}
	var out []GoalRequirement
	for _, line := range strings.Split(goal, "\n") {
		line = strings.TrimSpace(line)
		if len(line) < 3 || (line[0] != '-' && line[0] != '*') {
			continue
		}
		text := strings.TrimSpace(line[1:])
		if text == "" {
			continue
		}
		out = append(out, requirement("explore", "inspect "+text, GoalNodeExploration, "requested observation recorded", text))
	}
	return out
}

func commaInspectionRequirements(text string) []GoalRequirement {
	low := strings.ToLower(text)
	start := -1
	verb := ""
	for _, candidate := range []string{"inspect ", "collect ", "investigate "} {
		if i := strings.Index(low, candidate); i >= 0 && (start < 0 || i < start) {
			start, verb = i, strings.TrimSpace(candidate)
		}
	}
	if start < 0 {
		return nil
	}
	end := len(text)
	for _, marker := range []string{". determine", ". do not", ". return", "\n"} {
		if i := strings.Index(low[start:], marker); i >= 0 && start+i < end {
			end = start + i
		}
	}
	segment := strings.TrimSpace(text[start+len(verb)+1:end])
	lowSegment := strings.ToLower(segment)
	for _, action := range []string{" then ", " fix ", " implement ", " run ", " verify ", " return ", " make "} {
		if strings.Contains(lowSegment, action) {
			return nil
		}
	}
	parts := strings.Split(segment, ",")
	if len(parts) < 3 {
		return nil
	}
	out := make([]GoalRequirement, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		part = strings.TrimSpace(strings.TrimPrefix(strings.ToLower(part), "and "))
		if part == "" {
			continue
		}
		out = append(out, requirement("explore", verb+" "+part, GoalNodeExploration, "requested observation recorded", part))
	}
	if len(out) < 2 {
		return nil
	}
	return out
}

func requirement(base, description string, typ GoalNodeType, evidence, source string) GoalRequirement {
	return GoalRequirement{ID: base, Description: strings.TrimSpace(description), Type: typ, Required: true, EvidencePredicate: evidence, SourceText: strings.TrimSpace(source)}
}

func splitRequirementClauses(goal string) []string {
	// Preserve source wording while separating the common explicit sequencing
	// forms used in coding goals. The original full goal remains in each graph.
	goal = regexp.MustCompile(`(?i)\s+and\s+(create|write|update|add|fix|implement|run|inspect|trace|verify)\b`).ReplaceAllString(goal, `|$1`)
	return regexp.MustCompile(`(?i)\s+(?:then|and then)\s+|\s*\|\s*|\s*,\s*`).Split(goal, -1)
}

func fullGoTestMentioned(low string) bool {
	return strings.Contains(strings.Join(strings.Fields(low), " "), "go test ./...")
}
func relevantTestMentioned(low string) bool {
	low = strings.Join(strings.Fields(low), " ")
	return strings.Contains(low, "relevant test") || strings.Contains(low, "relevant tests")
}
func testCreationMentioned(low string) bool {
	return (strings.Contains(low, "add") || strings.Contains(low, "create") || strings.Contains(low, "write") || strings.Contains(low, "update")) && strings.Contains(low, "test")
}
func explorationMentioned(low string) bool {
	for _, phrase := range []string{"inspect ", "explore ", "trace ", "diagnose ", "locate ", "find ", "understand "} {
		if strings.Contains(low, phrase) {
			return true
		}
	}
	return false
}
func implementationMentioned(low string) bool {
	for _, word := range []string{"fix", "implement", "change", "refactor", "update", "add ", "create "} {
		if strings.Contains(low, word) {
			return true
		}
	}
	return false
}
func genericVerificationMentioned(low string) bool {
	for _, word := range []string{"verify", "verification", "validate", "check that", "run the test", "run tests", "execute tests"} {
		if strings.Contains(low, word) {
			return true
		}
	}
	return false
}
func artifactMentioned(text, low string) bool {
	return requirementArtifactPattern.MatchString(text) && (strings.Contains(low, "create") || strings.Contains(low, "write") || strings.Contains(low, "update") || strings.Contains(low, "report") || strings.Contains(low, "document"))
}

func stabilizeRequirementIDs(reqs []GoalRequirement) []GoalRequirement {
	counts := map[string]int{}
	for i := range reqs {
		reqs[i].Ordinal = i
		base := semanticRequirementID(reqs[i].ID, reqs[i].Description, reqs[i].Type)
		counts[base]++
		if counts[base] > 1 {
			reqs[i].ID = fmt.Sprintf("%s-%d", base, counts[base])
		} else {
			reqs[i].ID = base
		}
	}
	return reqs
}

func semanticRequirementID(base, description string, typ GoalNodeType) string {
	words := strings.Fields(strings.ToLower(description))
	var key string
	for _, word := range words {
		word = strings.Trim(word, "`'\".,:;()[]{}")
		if word == "" || word == "the" || word == "a" || word == "an" || word == "and" || word == "then" || word == "to" || word == "with" {
			continue
		}
		if len(word) > 32 {
			word = word[:32]
		}
		key += "-" + word
		if len(key) > 52 {
			break
		}
	}
	base = strings.Trim(strings.ToLower(base), "-")
	if key == "" {
		return base
	}
	return base + key
}

func sortRequirements(reqs []GoalRequirement) []GoalRequirement {
	out := append([]GoalRequirement(nil), reqs...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Ordinal < out[j].Ordinal })
	return out
}
