package agenthistory

import "testing"

// Ported from test/unit/history-has-advanced.test.ts.

func emptyHistory(source HistorySource) CompactAgentHistory {
	return EmptyCompactHistory(sourceString(source))
}

func TestHistoryHasAdvancedRecognizesANewAssistantReferenceEvenWhenTextIsUnchanged(t *testing.T) {
	baseline := emptyHistory(SourcePiJSONL)
	current := baseline
	current.LastAssistantMessage = &HistoryExcerpt{Ref: "entry-2", Text: "same"}
	changed := baseline
	changed.LastAssistantMessage = &HistoryExcerpt{Ref: "entry-1", Text: "same"}

	if !HistoryHasAdvanced(current, &changed, HistoryAdvancedOptions{}) {
		t.Fatal("historyHasAdvanced = false, want true for a new assistant reference")
	}
	if !HistoryHasAdvanced(current, &changed, HistoryAdvancedOptions{RequireAssistantChange: true}) {
		t.Fatal("historyHasAdvanced = false, want true for a new assistant reference (required change)")
	}
}

func TestHistoryHasAdvancedRecognizesMessageCountAdvancement(t *testing.T) {
	baseline := emptyHistory(SourcePiJSONL)
	baseline.MessageCount = 2
	current := baseline
	current.MessageCount = 3

	if !HistoryHasAdvanced(current, &baseline, HistoryAdvancedOptions{}) {
		t.Fatal("historyHasAdvanced = false, want true for a higher message count")
	}
}

func TestHistoryHasAdvancedDoesNotTreatEqualTextOrUnchangedCountAsAdvancement(t *testing.T) {
	baseline := emptyHistory(SourcePiJSONL)
	baseline.LastAssistantMessage = &HistoryExcerpt{Ref: "entry-1", Text: "same"}
	baseline.MessageCount = 2

	if HistoryHasAdvanced(baseline, &baseline, HistoryAdvancedOptions{}) {
		t.Fatal("historyHasAdvanced = true, want false for an unchanged history")
	}
	if HistoryHasAdvanced(baseline, &baseline, HistoryAdvancedOptions{RequireAssistantChange: true}) {
		t.Fatal("historyHasAdvanced = true, want false for an unchanged history (required change)")
	}
}

func TestHistoryHasAdvancedTreatsAnAbsentBaselineAsAdvancement(t *testing.T) {
	current := emptyHistory(SourcePiJSONL)
	current.LastAssistantMessage = &HistoryExcerpt{Ref: "entry-1", Text: "same"}
	current.MessageCount = 1

	if !HistoryHasAdvanced(current, nil, HistoryAdvancedOptions{}) {
		t.Fatal("historyHasAdvanced = false, want true when there is no baseline at all")
	}
	if !HistoryHasAdvanced(current, nil, HistoryAdvancedOptions{RequireAssistantChange: true}) {
		t.Fatal("historyHasAdvanced = false, want true when there is no baseline at all (required change)")
	}
}

func TestHistoryHasAdvancedRequiresAssistantChangeIgnoresMessageCount(t *testing.T) {
	baseline := emptyHistory(SourcePiJSONL)
	baseline.MessageCount = 2
	current := baseline
	current.MessageCount = 5

	if HistoryHasAdvanced(current, &baseline, HistoryAdvancedOptions{RequireAssistantChange: true}) {
		t.Fatal("historyHasAdvanced = true, want false when only the count moved and a change is required")
	}
	if !HistoryHasAdvanced(current, &baseline, HistoryAdvancedOptions{}) {
		t.Fatal("historyHasAdvanced = false, want true when only the count moved")
	}
}

func TestHistoryHasAdvancedIgnoresAnEmptyAssistantTextAgainstANewReference(t *testing.T) {
	baseline := emptyHistory(SourcePiJSONL)
	baseline.LastAssistantMessage = &HistoryExcerpt{Ref: "entry-1", Text: ""}
	current := baseline
	current.LastAssistantMessage = &HistoryExcerpt{Ref: "entry-1", Text: ""}

	if HistoryHasAdvanced(current, &baseline, HistoryAdvancedOptions{}) {
		t.Fatal("historyHasAdvanced = true, want false for identical empty assistant fields")
	}
}
