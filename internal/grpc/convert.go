package grpc

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/openjobspec/ojs-backend-sqs/internal/core"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"

	ojsv1 "github.com/openjobspec/ojs-proto/gen/go/ojs/v1"
)

// stateToProto maps core state strings to proto enum values.
var stateToProto = map[string]ojsv1.JobState{
	"scheduled": ojsv1.JobState_JOB_STATE_SCHEDULED,
	"available": ojsv1.JobState_JOB_STATE_AVAILABLE,
	"pending":   ojsv1.JobState_JOB_STATE_PENDING,
	"active":    ojsv1.JobState_JOB_STATE_ACTIVE,
	"completed": ojsv1.JobState_JOB_STATE_COMPLETED,
	"retryable": ojsv1.JobState_JOB_STATE_RETRYABLE,
	"cancelled": ojsv1.JobState_JOB_STATE_CANCELLED,
	"discarded": ojsv1.JobState_JOB_STATE_DISCARDED,
}

// jobToProto converts a core.Job to its protobuf representation.
func jobToProto(j *core.Job) *ojsv1.Job {
	if j == nil {
		return nil
	}

	pj := &ojsv1.Job{
		Id:      j.ID,
		Type:    j.Type,
		Queue:   j.Queue,
		State:   stateToProto[j.State],
		Attempt: int32(j.Attempt),
	}

	if j.Priority != nil {
		pj.Priority = int32(*j.Priority)
	}

	pj.Args = argsToProto(j.Args)
	pj.Meta = structFromJSON(j.Meta)
	pj.Result = structFromJSON(j.Result)

	pj.CreatedAt = parseRFC3339(j.CreatedAt)
	pj.EnqueuedAt = parseRFC3339(j.EnqueuedAt)
	pj.ScheduledAt = parseRFC3339(j.ScheduledAt)
	pj.StartedAt = parseRFC3339(j.StartedAt)
	pj.CompletedAt = parseRFC3339(j.CompletedAt)

	pj.RetryPolicy = retryPolicyToProto(j.Retry)
	pj.UniquePolicy = uniquePolicyToProto(j.Unique)

	return pj
}

// argsToProto converts a JSON args array into protobuf values, skipping any that
// cannot be represented.
func argsToProto(raw json.RawMessage) []*structpb.Value {
	if raw == nil {
		return nil
	}
	var args []any
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil
	}
	values := make([]*structpb.Value, 0, len(args))
	for _, a := range args {
		if v, err := structpb.NewValue(a); err == nil {
			values = append(values, v)
		}
	}
	return values
}

// structFromJSON converts a JSON object into a protobuf struct, returning nil for
// absent or malformed input.
func structFromJSON(raw json.RawMessage) *structpb.Struct {
	if raw == nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	s, err := structpb.NewStruct(m)
	if err != nil {
		return nil
	}
	return s
}

// retryPolicyToProto converts a core retry policy into its protobuf form.
func retryPolicyToProto(rp *core.RetryPolicy) *ojsv1.RetryPolicy {
	if rp == nil {
		return nil
	}
	out := &ojsv1.RetryPolicy{
		MaxAttempts:        int32(rp.MaxAttempts),
		BackoffCoefficient: rp.BackoffCoefficient,
		Jitter:             rp.Jitter,
	}
	if d, err := time.ParseDuration(rp.InitialInterval); err == nil {
		out.InitialInterval = durationpb.New(d)
	}
	if d, err := time.ParseDuration(rp.MaxInterval); err == nil {
		out.MaxInterval = durationpb.New(d)
	}
	return out
}

// uniquePolicyToProto converts a core unique policy into its protobuf form.
func uniquePolicyToProto(u *core.UniquePolicy) *ojsv1.UniquePolicy {
	if u == nil {
		return nil
	}
	out := &ojsv1.UniquePolicy{Key: u.Keys}
	if d, err := time.ParseDuration(u.Period); err == nil {
		out.Period = durationpb.New(d)
	}
	return out
}

// enqueueRequestToJob converts an EnqueueRequest to a core.Job.
func enqueueRequestToJob(req *ojsv1.EnqueueRequest) *core.Job {
	job := &core.Job{
		Type: req.Type,
	}

	if len(req.Args) > 0 {
		args := valuesToInterface(req.Args)
		if data, err := json.Marshal(args); err != nil {
			slog.Warn("failed to marshal gRPC args", "error", err)
		} else {
			job.Args = data
		}
	}

	if opts := req.Options; opts != nil {
		if opts.Queue != "" {
			job.Queue = opts.Queue
		}
		if opts.Priority != 0 {
			p := int(opts.Priority)
			job.Priority = &p
		}
		if opts.Meta != nil {
			if data, err := json.Marshal(opts.Meta.AsMap()); err != nil {
				slog.Warn("failed to marshal gRPC meta", "error", err)
			} else {
				job.Meta = data
			}
		}
		if opts.Retry != nil {
			job.Retry = protoRetryToCore(opts.Retry)
		}
		if opts.Unique != nil {
			job.Unique = protoUniqueToCore(opts.Unique)
		}
		if opts.DelayUntil != nil {
			job.ScheduledAt = opts.DelayUntil.AsTime().UTC().Format(time.RFC3339)
		}
	}

	return job
}

// enqueueJobRequestToJob converts a batch job entry to a core.Job.
func enqueueJobRequestToJob(req *ojsv1.BatchJobEntry) *core.Job {
	job := &core.Job{
		Type: req.Type,
	}

	if len(req.Args) > 0 {
		args := valuesToInterface(req.Args)
		if data, err := json.Marshal(args); err != nil {
			slog.Warn("failed to marshal gRPC args", "error", err)
		} else {
			job.Args = data
		}
	}

	if opts := req.Options; opts != nil {
		if opts.Queue != "" {
			job.Queue = opts.Queue
		}
		if opts.Priority != 0 {
			p := int(opts.Priority)
			job.Priority = &p
		}
		if opts.Meta != nil {
			if data, err := json.Marshal(opts.Meta.AsMap()); err != nil {
				slog.Warn("failed to marshal gRPC meta", "error", err)
			} else {
				job.Meta = data
			}
		}
	}

	return job
}

// stateToWorkflowProto maps core workflow state strings to proto enum values.
var stateToWorkflowProto = map[string]ojsv1.WorkflowState{
	"running":   ojsv1.WorkflowState_WORKFLOW_STATE_RUNNING,
	"completed": ojsv1.WorkflowState_WORKFLOW_STATE_COMPLETED,
	"failed":    ojsv1.WorkflowState_WORKFLOW_STATE_FAILED,
	"cancelled": ojsv1.WorkflowState_WORKFLOW_STATE_CANCELLED,
}

// workflowToProto converts a core.Workflow to its protobuf representation.
func workflowToProto(wf *core.Workflow) *ojsv1.Workflow {
	if wf == nil {
		return nil
	}

	pw := &ojsv1.Workflow{
		Id:        wf.ID,
		Name:      wf.Name,
		State:     stateToWorkflowProto[wf.State],
		CreatedAt: parseRFC3339(wf.CreatedAt),
	}

	if wf.CompletedAt != "" {
		pw.CompletedAt = parseRFC3339(wf.CompletedAt)
	}

	return pw
}

// protoToWorkflowRequest accepts the two DAG shapes represented by the HTTP
// workflow API: dependency-free groups and strict single-path chains.
func protoToWorkflowRequest(req *ojsv1.CreateWorkflowRequest) (*core.WorkflowRequest, error) {
	if req == nil || len(req.Steps) == 0 {
		return nil, fmt.Errorf("workflow requires at least one step")
	}
	dag, err := indexWorkflowSteps(req.Steps)
	if err != nil {
		return nil, err
	}
	independent, err := dag.connectDependencies(req.Steps)
	if err != nil {
		return nil, err
	}
	if independent {
		return dag.groupRequest(req.Name), nil
	}
	return dag.linearRequest(req.Name)
}

type workflowDAG struct {
	steps    map[string]*ojsv1.WorkflowStep
	order    []string
	outgoing map[string][]string
	indegree map[string]int
}

func indexWorkflowSteps(steps []*ojsv1.WorkflowStep) (*workflowDAG, error) {
	dag := &workflowDAG{
		steps:    make(map[string]*ojsv1.WorkflowStep, len(steps)),
		order:    make([]string, 0, len(steps)),
		outgoing: make(map[string][]string, len(steps)),
		indegree: make(map[string]int, len(steps)),
	}
	for i, step := range steps {
		if step == nil || step.Id == "" {
			return nil, fmt.Errorf("workflow step %d requires an id", i)
		}
		if step.Type == "" {
			return nil, fmt.Errorf("workflow step %q requires a type", step.Id)
		}
		if _, exists := dag.steps[step.Id]; exists {
			return nil, fmt.Errorf("duplicate workflow step id %q", step.Id)
		}
		dag.steps[step.Id] = step
		dag.order = append(dag.order, step.Id)
		dag.indegree[step.Id] = len(step.DependsOn)
	}
	return dag, nil
}

func (d *workflowDAG) connectDependencies(steps []*ojsv1.WorkflowStep) (bool, error) {
	allIndependent := true
	for _, step := range steps {
		if len(step.DependsOn) > 0 {
			allIndependent = false
		}
		if err := d.connectStep(step); err != nil {
			return false, err
		}
	}
	return allIndependent, nil
}

func (d *workflowDAG) connectStep(step *ojsv1.WorkflowStep) error {
	seen := make(map[string]bool, len(step.DependsOn))
	for _, dependency := range step.DependsOn {
		if dependency == step.Id {
			return fmt.Errorf("workflow step %q cannot depend on itself", step.Id)
		}
		if seen[dependency] {
			return fmt.Errorf("workflow step %q repeats dependency %q", step.Id, dependency)
		}
		seen[dependency] = true
		if _, exists := d.steps[dependency]; !exists {
			return fmt.Errorf("workflow step %q depends on unknown step %q", step.Id, dependency)
		}
		d.outgoing[dependency] = append(d.outgoing[dependency], step.Id)
	}
	return nil
}

func (d *workflowDAG) groupRequest(name string) *core.WorkflowRequest {
	request := &core.WorkflowRequest{Type: "group", Name: name}
	for _, id := range d.order {
		request.Jobs = append(request.Jobs, protoToWorkflowStep(d.steps[id]))
	}
	return request
}

func (d *workflowDAG) linearRequest(name string) (*core.WorkflowRequest, error) {
	if err := validateWorkflowDAG(d.order, d.indegree, d.outgoing); err != nil {
		return nil, err
	}
	root, err := d.linearRoot()
	if err != nil {
		return nil, err
	}
	request := &core.WorkflowRequest{Type: "chain", Name: name}
	visited := make(map[string]bool, len(d.steps))
	for current := root; current != ""; current = onlyChild(d.outgoing[current]) {
		visited[current] = true
		request.Steps = append(request.Steps, protoToWorkflowStep(d.steps[current]))
	}
	if len(visited) != len(d.steps) {
		return nil, fmt.Errorf("workflow DAG is valid but unsupported; all steps must form one linear chain")
	}
	return request, nil
}

func (d *workflowDAG) linearRoot() (string, error) {
	root := ""
	for id, degree := range d.indegree {
		if degree > 1 || len(d.outgoing[id]) > 1 {
			return "", fmt.Errorf("workflow DAG is valid but unsupported; only a strict linear chain or dependency-free group is supported")
		}
		if degree == 0 {
			if root != "" {
				return "", fmt.Errorf("workflow DAG is not a strict linear chain")
			}
			root = id
		}
	}
	if root == "" {
		return "", fmt.Errorf("workflow DAG is not a strict linear chain")
	}
	return root, nil
}

func onlyChild(children []string) string {
	if len(children) == 0 {
		return ""
	}
	return children[0]
}

func validateWorkflowDAG(order []string, indegree map[string]int, outgoing map[string][]string) error {
	degrees := make(map[string]int, len(indegree))
	queue := make([]string, 0, len(order))
	for _, id := range order {
		degrees[id] = indegree[id]
		if indegree[id] == 0 {
			queue = append(queue, id)
		}
	}
	visited := 0
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		visited++
		for _, child := range outgoing[id] {
			degrees[child]--
			if degrees[child] == 0 {
				queue = append(queue, child)
			}
		}
	}
	if visited != len(order) {
		return fmt.Errorf("workflow contains a dependency cycle")
	}
	return nil
}

// protoToWorkflowStep converts a protobuf workflow step to a core.WorkflowJobRequest.
func protoToWorkflowStep(req *ojsv1.WorkflowStep) core.WorkflowJobRequest {
	wj := core.WorkflowJobRequest{
		Name: req.Id,
		Type: req.Type,
	}

	if len(req.Args) > 0 {
		args := valuesToInterface(req.Args)
		if data, err := json.Marshal(args); err != nil {
			slog.Warn("failed to marshal gRPC workflow step args", "error", err)
		} else {
			wj.Args = data
		}
	}
	wj.Options = protoWorkflowOptions(req.Options)

	return wj
}

func protoWorkflowOptions(options *ojsv1.EnqueueOptions) *core.EnqueueOptions {
	if options == nil {
		return nil
	}
	out := &core.EnqueueOptions{
		Queue:  options.Queue,
		Tags:   append([]string(nil), options.Tags...),
		Retry:  protoRetryToCore(options.Retry),
		Unique: protoUniqueToCore(options.Unique),
	}
	if options.Priority != 0 {
		priority := int(options.Priority)
		out.Priority = &priority
	}
	if options.DelayUntil != nil {
		out.DelayUntil = options.DelayUntil.AsTime().UTC().Format(time.RFC3339Nano)
	}
	if options.Timeout != nil {
		timeoutMs := int(options.Timeout.AsDuration().Milliseconds())
		out.TimeoutMs = &timeoutMs
	}
	if options.Ttl != nil {
		out.ExpiresAt = time.Now().Add(options.Ttl.AsDuration()).UTC().Format(time.RFC3339Nano)
	}
	if options.VisibilityTimeout != nil {
		visibilityMs := int(options.VisibilityTimeout.AsDuration().Milliseconds())
		out.VisibilityTimeoutMs = &visibilityMs
	}
	if options.Meta != nil {
		if data, err := json.Marshal(options.Meta.AsMap()); err == nil {
			out.Metadata = data
		}
	}
	if out.Retry != nil {
		out.RetryPolicy = out.Retry
	}
	return out
}

// protoRetryToCore converts a proto RetryPolicy to a core RetryPolicy.
func protoRetryToCore(r *ojsv1.RetryPolicy) *core.RetryPolicy {
	if r == nil {
		return nil
	}
	cr := &core.RetryPolicy{
		MaxAttempts:        int(r.MaxAttempts),
		BackoffCoefficient: r.BackoffCoefficient,
		Jitter:             r.Jitter,
	}
	if r.InitialInterval != nil {
		cr.InitialInterval = r.InitialInterval.AsDuration().String()
	}
	if r.MaxInterval != nil {
		cr.MaxInterval = r.MaxInterval.AsDuration().String()
	}
	return cr
}

// protoUniqueToCore converts a proto UniquePolicy to a core UniquePolicy.
func protoUniqueToCore(u *ojsv1.UniquePolicy) *core.UniquePolicy {
	if u == nil {
		return nil
	}
	cu := &core.UniquePolicy{
		Keys: u.Key,
	}
	if u.Period != nil {
		cu.Period = u.Period.AsDuration().String()
	}
	switch u.OnConflict {
	case ojsv1.UniqueConflictAction_UNIQUE_CONFLICT_ACTION_REJECT:
		cu.OnConflict = "reject"
	case ojsv1.UniqueConflictAction_UNIQUE_CONFLICT_ACTION_REPLACE:
		cu.OnConflict = "replace"
	case ojsv1.UniqueConflictAction_UNIQUE_CONFLICT_ACTION_IGNORE:
		cu.OnConflict = "ignore"
	case ojsv1.UniqueConflictAction_UNIQUE_CONFLICT_ACTION_REPLACE_EXCEPT_SCHEDULE:
		cu.OnConflict = "replace"
	}
	for _, state := range u.States {
		for name, candidate := range stateToProto {
			if candidate == state {
				cu.States = append(cu.States, name)
				break
			}
		}
	}
	return cu
}
