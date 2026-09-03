package grpc

import (
	"encoding/json"
	"testing"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
	ojsv1 "github.com/openjobspec/ojs-proto/gen/go/ojs/v1"
)

func intPtrVal(v int) *int { return &v }

func TestJobToProto_Nil(t *testing.T) {
	if got := jobToProto(nil); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

func sampleJob() *core.Job {
	priority := 5
	return &core.Job{
		ID:          "job-1",
		Type:        "email.send",
		Queue:       "default",
		State:       core.StateActive,
		Attempt:     2,
		Priority:    intPtrVal(priority),
		Args:        json.RawMessage(`["alice", 42]`),
		Meta:        json.RawMessage(`{"tenant":"acme"}`),
		Result:      json.RawMessage(`{"sent":true}`),
		CreatedAt:   "2025-01-01T10:00:00.000Z",
		EnqueuedAt:  "2025-01-01T10:00:01.000Z",
		StartedAt:   "2025-01-01T10:00:02.000Z",
		CompletedAt: "2025-01-01T10:00:03.000Z",
		Retry: &core.RetryPolicy{
			MaxAttempts:        4,
			BackoffCoefficient: 2.0,
			Jitter:             true,
			InitialInterval:    "5s",
			MaxInterval:        "30s",
		},
		Unique: &core.UniquePolicy{
			Keys:   []string{"type", "args"},
			Period: "10s",
		},
	}
}

func TestJobToProto_CoreFields(t *testing.T) {
	pj := jobToProto(sampleJob())
	if pj == nil {
		t.Fatal("expected non-nil proto job")
	}
	if pj.Id != "job-1" {
		t.Errorf("id = %q", pj.Id)
	}
	if pj.Type != "email.send" {
		t.Errorf("type = %q", pj.Type)
	}
	if pj.Queue != "default" {
		t.Errorf("queue = %q", pj.Queue)
	}
	if pj.State != ojsv1.JobState_JOB_STATE_ACTIVE {
		t.Errorf("state = %v, want ACTIVE", pj.State)
	}
	if pj.Attempt != 2 {
		t.Errorf("attempt = %d, want 2", pj.Attempt)
	}
	if pj.Priority != 5 {
		t.Errorf("priority = %d, want 5", pj.Priority)
	}
}

func TestJobToProto_PayloadFields(t *testing.T) {
	pj := jobToProto(sampleJob())
	if len(pj.Args) != 2 {
		t.Fatalf("args len = %d, want 2", len(pj.Args))
	}
	if pj.Args[0].GetStringValue() != "alice" {
		t.Errorf("args[0] = %v, want alice", pj.Args[0])
	}
	if pj.Meta == nil || pj.Meta.Fields["tenant"].GetStringValue() != "acme" {
		t.Errorf("meta not mapped: %v", pj.Meta)
	}
	if pj.Result == nil || !pj.Result.Fields["sent"].GetBoolValue() {
		t.Errorf("result not mapped: %v", pj.Result)
	}
}

func TestJobToProto_Timestamps(t *testing.T) {
	pj := jobToProto(sampleJob())
	if pj.CreatedAt == nil || pj.EnqueuedAt == nil || pj.StartedAt == nil || pj.CompletedAt == nil {
		t.Error("expected timestamps to be populated")
	}
}

func TestJobToProto_Policies(t *testing.T) {
	pj := jobToProto(sampleJob())
	if pj.RetryPolicy == nil || pj.RetryPolicy.MaxAttempts != 4 || pj.RetryPolicy.InitialInterval == nil {
		t.Errorf("retry policy not mapped: %v", pj.RetryPolicy)
	}
	if pj.UniquePolicy == nil || len(pj.UniquePolicy.Key) != 2 || pj.UniquePolicy.Period == nil {
		t.Errorf("unique policy not mapped: %v", pj.UniquePolicy)
	}
}

func TestJobToProto_MinimalJob(t *testing.T) {
	pj := jobToProto(&core.Job{ID: "j", Type: "t", Queue: "q", State: core.StateAvailable})
	if pj == nil {
		t.Fatal("expected non-nil")
	}
	if pj.State != ojsv1.JobState_JOB_STATE_AVAILABLE {
		t.Errorf("state = %v, want AVAILABLE", pj.State)
	}
	if pj.RetryPolicy != nil || pj.UniquePolicy != nil {
		t.Error("expected no retry/unique policy for minimal job")
	}
	if len(pj.Args) != 0 {
		t.Errorf("expected no args, got %d", len(pj.Args))
	}
}

func TestEventMatchesFilters(t *testing.T) {
	ev := &core.JobEvent{Queue: "emails", EventType: "job.completed"}

	tests := []struct {
		name        string
		queueFilter map[string]bool
		typeFilter  map[string]bool
		want        bool
	}{
		{"no filters", nil, nil, true},
		{"matching queue", map[string]bool{"emails": true}, nil, true},
		{"non-matching queue", map[string]bool{"other": true}, nil, false},
		{"matching type", nil, map[string]bool{"job.completed": true}, true},
		{"non-matching type", nil, map[string]bool{"job.failed": true}, false},
		{"matching both", map[string]bool{"emails": true}, map[string]bool{"job.completed": true}, true},
		{"queue ok type mismatch", map[string]bool{"emails": true}, map[string]bool{"job.failed": true}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := eventMatchesFilters(ev, tt.queueFilter, tt.typeFilter); got != tt.want {
				t.Errorf("eventMatchesFilters = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestProtoToWorkflowRequest_DependencyFreeBecomesGroup(t *testing.T) {
	request, err := protoToWorkflowRequest(&ojsv1.CreateWorkflowRequest{
		Name: "parallel",
		Steps: []*ojsv1.WorkflowStep{
			{Id: "a", Type: "job.a"},
			{Id: "b", Type: "job.b"},
		},
	})
	if err != nil {
		t.Fatalf("protoToWorkflowRequest: %v", err)
	}
	if request.Type != "group" || len(request.Jobs) != 2 {
		t.Fatalf("request = %#v, want two-job group", request)
	}
}

func TestProtoToWorkflowRequest_StrictLinearBecomesOrderedChainWithOptions(t *testing.T) {
	request, err := protoToWorkflowRequest(&ojsv1.CreateWorkflowRequest{
		Name: "linear",
		Steps: []*ojsv1.WorkflowStep{
			{Id: "third", Type: "job.third", DependsOn: []string{"second"}},
			{Id: "first", Type: "job.first", Options: &ojsv1.EnqueueOptions{Queue: "critical"}},
			{Id: "second", Type: "job.second", DependsOn: []string{"first"}},
		},
	})
	if err != nil {
		t.Fatalf("protoToWorkflowRequest: %v", err)
	}
	if request.Type != "chain" || len(request.Steps) != 3 {
		t.Fatalf("request = %#v, want three-step chain", request)
	}
	if request.Steps[0].Name != "first" || request.Steps[1].Name != "second" || request.Steps[2].Name != "third" {
		t.Fatalf("chain order = %q, %q, %q", request.Steps[0].Name, request.Steps[1].Name, request.Steps[2].Name)
	}
	if request.Steps[0].Options == nil || request.Steps[0].Options.Queue != "critical" {
		t.Fatalf("step options were not preserved: %#v", request.Steps[0].Options)
	}
}

func TestProtoToWorkflowRequest_RejectsInvalidAndUnsupportedDAGs(t *testing.T) {
	tests := []struct {
		name  string
		steps []*ojsv1.WorkflowStep
	}{
		{name: "zero steps"},
		{
			name: "cycle",
			steps: []*ojsv1.WorkflowStep{
				{Id: "a", Type: "job.a", DependsOn: []string{"b"}},
				{Id: "b", Type: "job.b", DependsOn: []string{"a"}},
			},
		},
		{
			name: "unknown dependency",
			steps: []*ojsv1.WorkflowStep{
				{Id: "a", Type: "job.a", DependsOn: []string{"missing"}},
			},
		},
		{
			name: "branching DAG",
			steps: []*ojsv1.WorkflowStep{
				{Id: "root", Type: "job.root"},
				{Id: "left", Type: "job.left", DependsOn: []string{"root"}},
				{Id: "right", Type: "job.right", DependsOn: []string{"root"}},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := protoToWorkflowRequest(&ojsv1.CreateWorkflowRequest{Steps: test.steps}); err == nil {
				t.Fatal("expected DAG validation error")
			}
		})
	}
}

func TestStringSet(t *testing.T) {
	if stringSet(nil) != nil {
		t.Error("expected nil for empty input")
	}
	set := stringSet([]string{"a", "b", "a"})
	if len(set) != 2 || !set["a"] || !set["b"] {
		t.Errorf("unexpected set: %v", set)
	}
}
