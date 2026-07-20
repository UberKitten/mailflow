package cli

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"mailflow/internal/config"
	"mailflow/internal/engine"
)

type fakeExactProcessor struct {
	events *[]string
	result engine.ProcessResult
	err    error
}

func (f fakeExactProcessor) ProcessMessage(_ context.Context, messageID string) (engine.ProcessResult, error) {
	*f.events = append(*f.events, "process:"+messageID)
	return f.result, f.err
}

type fakeCategoryRemover struct {
	events    *[]string
	remaining []string
	err       error
}

func (f fakeCategoryRemover) RemoveCategory(_ context.Context, messageID, category string) ([]string, error) {
	*f.events = append(*f.events, "clear:"+messageID+":"+category)
	return f.remaining, f.err
}

func TestProcessAndClearCategoryUsesMovedIDAfterProcessing(t *testing.T) {
	events := []string{}
	processor := fakeExactProcessor{
		events: &events,
		result: engine.ProcessResult{
			MessageID:   "moved-id",
			Moved:       true,
			MatchedRule: &config.Rule{Name: "security", Folder: "Inbox/Security"},
		},
	}
	remover := fakeCategoryRemover{
		events:    &events,
		remaining: []string{"Keep", "Sort → Other"},
	}

	processed, remaining, err := processAndClearCategory(
		context.Background(),
		processor,
		remover,
		"original-id",
		"Sort → Inbox/Security",
	)
	if err != nil {
		t.Fatalf("processAndClearCategory: %v", err)
	}
	if processed.MessageID != "moved-id" {
		t.Fatalf("processed message ID = %q, want moved-id", processed.MessageID)
	}
	if !reflect.DeepEqual(remaining, []string{"Keep", "Sort → Other"}) {
		t.Fatalf("remaining = %v", remaining)
	}
	wantEvents := []string{
		"process:original-id",
		"clear:moved-id:Sort → Inbox/Security",
	}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("events = %v, want %v", events, wantEvents)
	}
}

func TestProcessAndClearCategoryDoesNotClearAfterProcessingFailure(t *testing.T) {
	events := []string{}
	processErr := errors.New("move failed")
	_, _, err := processAndClearCategory(
		context.Background(),
		fakeExactProcessor{events: &events, err: processErr},
		fakeCategoryRemover{events: &events},
		"original-id",
		"Missort",
	)
	if !errors.Is(err, processErr) {
		t.Fatalf("error = %v, want processing failure", err)
	}
	if !reflect.DeepEqual(events, []string{"process:original-id"}) {
		t.Fatalf("events = %v, category clear must not run", events)
	}
}

func TestProcessAndClearCategoryAllowsOrdinaryNoMatchWithoutCorrection(t *testing.T) {
	events := []string{}
	want := engine.ProcessResult{MessageID: "original-id"}
	got, remaining, err := processAndClearCategory(
		context.Background(),
		fakeExactProcessor{events: &events, result: want},
		fakeCategoryRemover{events: &events},
		"original-id",
		"",
	)
	if err != nil {
		t.Fatalf("ordinary no-match processing returned error: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("result = %+v, want %+v", got, want)
	}
	if remaining != nil {
		t.Fatalf("remaining = %v, want nil without category clear", remaining)
	}
	if !reflect.DeepEqual(events, []string{"process:original-id"}) {
		t.Fatalf("events = %v, category clear must not run", events)
	}
}

func TestProcessAndClearCategoryDoesNotClearWithoutSuccessfulMove(t *testing.T) {
	tests := []struct {
		name      string
		result    engine.ProcessResult
		wantError string
	}{
		{
			name:      "no matching rule",
			result:    engine.ProcessResult{MessageID: "original-id"},
			wantError: "no sorting rule matched; message was not moved",
		},
		{
			name: "matched but not moved",
			result: engine.ProcessResult{
				MessageID:   "original-id",
				MatchedRule: &config.Rule{Name: "promotions"},
			},
			wantError: `rule "promotions" matched but message was not moved`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events := []string{}
			_, _, err := processAndClearCategory(
				context.Background(),
				fakeExactProcessor{events: &events, result: tt.result},
				fakeCategoryRemover{events: &events},
				"original-id",
				"Sort → Promotions",
			)
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("error = %v, want actionable error containing %q", err, tt.wantError)
			}
			if !reflect.DeepEqual(events, []string{"process:original-id"}) {
				t.Fatalf("events = %v, category clear must not run", events)
			}
		})
	}
}

func TestProcessAndClearCategoryReturnsClearFailure(t *testing.T) {
	events := []string{}
	clearErr := errors.New("graph patch rejected")
	_, _, err := processAndClearCategory(
		context.Background(),
		fakeExactProcessor{
			events: &events,
			result: engine.ProcessResult{
				MessageID:   "moved-id",
				Moved:       true,
				MatchedRule: &config.Rule{Name: "promotions", Folder: "Promotions"},
			},
		},
		fakeCategoryRemover{events: &events, err: clearErr},
		"original-id",
		"Sort → Promotions",
	)
	if !errors.Is(err, clearErr) {
		t.Fatalf("error = %v, want category clear failure", err)
	}
	if !strings.Contains(err.Error(), `remove category "Sort → Promotions" from processed message "moved-id"`) {
		t.Fatalf("error is not actionable: %v", err)
	}
}
