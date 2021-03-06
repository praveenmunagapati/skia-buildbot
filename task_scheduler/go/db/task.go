package db

import (
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.skia.org/infra/go/common"
	"go.skia.org/infra/go/swarming"
	"go.skia.org/infra/go/util"

	swarming_api "go.chromium.org/luci/common/api/swarming/swarming/v1"
	"go.skia.org/infra/go/sklog"
)

const (
	// Swarming tags added by Task Scheduler.
	SWARMING_TAG_ATTEMPT          = "sk_attempt"
	SWARMING_TAG_DIMENSION_PREFIX = "sk_dim_"
	SWARMING_TAG_FORCED_JOB_ID    = "sk_forced_job_id"
	SWARMING_TAG_ID               = "sk_id"
	SWARMING_TAG_ISSUE            = "sk_issue"
	SWARMING_TAG_LUCI_PROJECT     = "luci_project"
	SWARMING_TAG_NAME             = "sk_name"
	SWARMING_TAG_PARENT_TASK_ID   = "sk_parent_task_id"
	SWARMING_TAG_PATCHSET         = "sk_patchset"
	SWARMING_TAG_PRIORITY         = "sk_priority"
	SWARMING_TAG_REPO             = "sk_repo"
	SWARMING_TAG_RETRY_OF         = "sk_retry_of"
	SWARMING_TAG_REVISION         = "sk_revision"
	SWARMING_TAG_SERVER           = "sk_issue_server"

	// These two tags allow the swarming ui to point to the GoB repo
	SWARMING_TAG_SOURCE_REVISION = "source_revision"
	SWARMING_TAG_SOURCE_REPO     = "source_repo"
)

type TaskStatus string

const (
	// TASK_STATUS_PENDING indicates the task has not started. It is the empty
	// string so that it is the zero value of TaskStatus.
	TASK_STATUS_PENDING TaskStatus = ""
	// TASK_STATUS_RUNNING indicates the task is in progress.
	TASK_STATUS_RUNNING TaskStatus = "RUNNING"
	// TASK_STATUS_SUCCESS indicates the task completed successfully.
	TASK_STATUS_SUCCESS TaskStatus = "SUCCESS"
	// TASK_STATUS_FAILURE indicates the task completed with failures.
	TASK_STATUS_FAILURE TaskStatus = "FAILURE"
	// TASK_STATUS_MISHAP indicates the task exited early with an error, died
	// while in progress, was manually canceled, expired while waiting on the
	// queue, or timed out before completing.
	TASK_STATUS_MISHAP TaskStatus = "MISHAP"
)

// TaskKey is a struct used for identifying a Task instance. Note that more
// than one Task may have the same TaskKey, eg. in the case of retries.
type TaskKey struct {
	RepoState
	Name        string `json:"name"`
	ForcedJobId string `json:"forcedJobId"`
}

// Copy returns a copy of the TaskKey.
func (k TaskKey) Copy() TaskKey {
	return TaskKey{
		RepoState:   k.RepoState.Copy(),
		Name:        k.Name,
		ForcedJobId: k.ForcedJobId,
	}
}

// Valid indicates whether or not the TaskKey is valid.
func (k TaskKey) Valid() bool {
	return k.RepoState.Valid() && k.Name != ""
}

// IsForceRun indicates whether this Task is for a forced Job, which
// indicates that it shouldn't be de-duplicated.
func (k TaskKey) IsForceRun() bool {
	return k.ForcedJobId != ""
}

// Task describes a Swarming task generated from a TaskSpec, or a "fake" task
// that can not be executed on Swarming, but can be added to the DB and
// displayed as if it were a real TaskSpec.
//
// Task is stored as a GOB, so changes must maintain backwards compatibility.
// See gob package documentation for details, but generally:
//   - Ensure new fields can be initialized with their zero value.
//   - Do not change the type of any existing field.
//   - Leave removed fields commented out to ensure the field name is not
//     reused.
//   - Add any new fields to the Copy() method.
type Task struct {
	// Attempt is the attempt number of this task, starting with zero.
	Attempt int `json:"attempt"`

	// Commits are the commits which were tested in this Task. The list may
	// change due to backfilling/bisecting.
	Commits []string `json:"commits"`

	// Created is the creation timestamp.
	Created time.Time `json:"created"`

	// DbModified is the time of the last successful call to TaskDB.PutTask/s for this
	// Task, or zero if the task is new. It is not related to the ModifiedTs time
	// of the associated Swarming task.
	DbModified time.Time `json:"dbModified"`

	// Finished is the time the task stopped running or expired from the queue, or
	// zero if the task is pending or running.
	Finished time.Time `json:"finished"`

	// Id is a generated unique identifier for this Task instance. Must be
	// URL-safe.
	Id string `json:"id"`

	// IsolatedOutput is the isolated hash of any outputs produced by this Task.
	// Filled in when the task is completed. This field will not be set if the
	// Task does not correspond to a Swarming task.
	IsolatedOutput string `json:"isolatedOutput"`

	// Jobs are the IDs of all Jobs which utilized this Task.
	Jobs []string `json:"jobs"`

	// MaxAttempts is the maximum number of attempts for this TaskSpec.
	MaxAttempts int `json:"max_attempts"`

	// ParentTaskIds are IDs of tasks which satisfied this task's dependencies.
	ParentTaskIds []string `json:"parentTaskIds"`

	// Properties contains key-value pairs from external sources. Both key and
	// value must be UTF-8 strings. Prefer a JavaScript identifier for key. Use
	// base64 encoding for binary data.
	Properties map[string]string `json:"properties"`

	// RetryOf is the ID of the task which this task is a retry of, if any.
	RetryOf string `json:"retryOf"`

	// Started is the time the task started running, or zero if the task is
	// pending, or the same as Finished if the task never ran.
	Started time.Time `json:"started"`

	// Status is the current task status, default TASK_STATUS_PENDING.
	Status TaskStatus `json:"status"`

	// SwarmingBotId is the ID of the Swarming slave that ran this task. This
	// field will not be set if the Task does not correspond to a Swarming task or
	// if the task is still pending.
	SwarmingBotId string `json:"swarmingBotId"`

	// SwarmingTaskId is the Swarming task ID. This field will not be set if the
	// Task does not correspond to a Swarming task.
	SwarmingTaskId string `json:"swarmingTaskId"`

	// TaskKey is a struct which describes aspects of the Task related
	// to the current state of the repo when it ran, and about the Task
	// itself.
	TaskKey
}

// UpdateFromSwarming sets or initializes t from data in s. If any changes were
// made to t, returns true.
//
// Returns ErrUnknownId if the SwarmingTaskId does not match.
//
// If empty, sets t.Id, t.Name, t.Repo, and t.Revision from s's tags named
// SWARMING_TAG_ID, SWARMING_TAG_NAME, SWARMING_TAG_REPO, and
// SWARMING_TAG_REVISION, and sets t.Created from s.CreatedTs. If these fields
// are non-empty, returns an error if they do not match.
//
// Always sets t.Status, t.Started, t.Finished, and t.IsolatedOutput based on s.
func (orig *Task) UpdateFromSwarming(s *swarming_api.SwarmingRpcsTaskResult) (bool, error) {
	if s == nil {
		return false, fmt.Errorf("Missing TaskResult. %v", s)
	}

	// Swarming TaskId.
	if orig.SwarmingTaskId != s.TaskId {
		return false, ErrUnknownId
	}

	tags, err := swarming.ParseTags(s.Tags)
	if err != nil {
		return false, err
	}

	copy := orig.Copy()
	if !reflect.DeepEqual(orig, copy) {
		sklog.Fatalf("Task.Copy is broken; original and copy differ:\n%#v\n%#v", orig, copy)
	}

	// "Identity" fields stored in tags.
	checkOrSetFromTag := func(tagName string, field *string, fieldName string) error {
		if tagValue, ok := tags[tagName]; ok {
			if len(tagValue) != 1 {
				return fmt.Errorf("Expected a single value for tag key %q", tagName)
			}
			if *field == "" {
				*field = tagValue[0]
			} else if *field != tagValue[0] {
				return fmt.Errorf("%s does not match for task %s. Was %s, now %s. %v %v", fieldName, orig.Id, *field, tagValue, orig, s)
			}
		}
		return nil
	}
	if err := checkOrSetFromTag(SWARMING_TAG_FORCED_JOB_ID, &copy.ForcedJobId, "ForcedJobId"); err != nil {
		return false, err
	}
	if err := checkOrSetFromTag(SWARMING_TAG_ID, &copy.Id, "Id"); err != nil {
		return false, err
	}
	if err := checkOrSetFromTag(SWARMING_TAG_NAME, &copy.Name, "Name"); err != nil {
		return false, err
	}
	if err := checkOrSetFromTag(SWARMING_TAG_REPO, &copy.Repo, "Repo"); err != nil {
		return false, err
	}
	if err := checkOrSetFromTag(SWARMING_TAG_RETRY_OF, &copy.RetryOf, "RetryOf"); err != nil {
		return false, err
	}
	if err := checkOrSetFromTag(SWARMING_TAG_REVISION, &copy.Revision, "Revision"); err != nil {
		return false, err
	}
	attempt := fmt.Sprintf("%d", copy.Attempt)
	if err := checkOrSetFromTag(SWARMING_TAG_ATTEMPT, &attempt, "Attempt"); err != nil {
		return false, err
	}
	attemptInt, err := strconv.ParseInt(attempt, 10, 32)
	if err != nil {
		return false, fmt.Errorf("Failed to ParseInt: %s", err)
	}
	copy.Attempt = int(attemptInt)
	if orig.Attempt != 0 && copy.Attempt != orig.Attempt {
		return false, fmt.Errorf("Attempt does not match for task %s. Was %d now %d. %v %v", orig.Id, orig.Attempt, copy.Attempt, orig, s)
	}

	// Optional try job tags.
	if err := checkOrSetFromTag(SWARMING_TAG_ISSUE, &copy.Issue, "Issue"); err != nil {
		return false, err
	}
	if err := checkOrSetFromTag(SWARMING_TAG_PATCHSET, &copy.Patchset, "Patchset"); err != nil {
		return false, err
	}
	if err := checkOrSetFromTag(SWARMING_TAG_SERVER, &copy.Server, "Server"); err != nil {
		return false, err
	}

	// Set ParentTaskIds.
	parentTaskIds := tags[SWARMING_TAG_PARENT_TASK_ID]
	sort.Strings(parentTaskIds)
	copy.ParentTaskIds = parentTaskIds

	// CreatedTs should always be present.
	if sCreated, err := swarming.ParseTimestamp(s.CreatedTs); err == nil {
		if util.TimeIsZero(copy.Created) {
			copy.Created = sCreated
		} else if copy.Created != sCreated {
			return false, fmt.Errorf("Creation time has changed for task %s. Was %s, now %s. %v", orig.Id, orig.Created, sCreated, orig)
		}
	} else {
		return false, fmt.Errorf("Unable to parse task creation time for task %s. %v %v", orig.Id, err, orig)
	}

	// Status.
	switch s.State {
	case swarming.TASK_STATE_BOT_DIED, swarming.TASK_STATE_CANCELED, swarming.TASK_STATE_EXPIRED, swarming.TASK_STATE_TIMED_OUT:
		copy.Status = TASK_STATUS_MISHAP
	case swarming.TASK_STATE_PENDING:
		copy.Status = TASK_STATUS_PENDING
	case swarming.TASK_STATE_RUNNING:
		copy.Status = TASK_STATUS_RUNNING
	case swarming.TASK_STATE_COMPLETED:
		if s.Failure {
			// TODO(benjaminwagner): Choose FAILURE or MISHAP depending on ExitCode?
			copy.Status = TASK_STATUS_FAILURE
		} else {
			copy.Status = TASK_STATUS_SUCCESS
		}
	default:
		return false, fmt.Errorf("Unknown Swarming State %v in %v", s.State, s)
	}

	// Isolated output.
	if s.OutputsRef == nil {
		copy.IsolatedOutput = ""
	} else {
		copy.IsolatedOutput = s.OutputsRef.Isolated
	}

	// Bot.
	copy.SwarmingBotId = s.BotId

	// Timestamps.
	maybeUpdateTime := func(newTimeStr string, field *time.Time, name string) error {
		if newTimeStr == "" {
			return nil
		}
		newTime, err := swarming.ParseTimestamp(newTimeStr)
		if err != nil {
			return fmt.Errorf("Unable to parse %s for task %s. %v %v", name, orig.Id, err, s)
		}
		*field = newTime
		return nil
	}

	if err := maybeUpdateTime(s.StartedTs, &copy.Started, "StartedTs"); err != nil {
		return false, err
	}
	if err := maybeUpdateTime(s.CompletedTs, &copy.Finished, "CompletedTs"); err != nil {
		return false, err
	}
	if s.CompletedTs == "" && copy.Status == TASK_STATUS_MISHAP {
		if err := maybeUpdateTime(s.AbandonedTs, &copy.Finished, "AbandonedTs"); err != nil {
			return false, err
		}
	}
	if copy.Done() && util.TimeIsZero(copy.Started) {
		copy.Started = copy.Finished
	}

	// TODO(benjaminwagner): SwarmingRpcsTaskResult has a ModifiedTs field that we
	// could use to detect modifications. Unfortunately, it seems that while the
	// task is running, ModifiedTs gets updated every 30 seconds, regardless of
	// whether any other data actually changed. Maybe we could still use it for
	// pending or completed tasks.
	if !reflect.DeepEqual(orig, copy) {
		*orig = *copy
		return true, nil
	}
	return false, nil
}

var errNotModified = errors.New("Task not modified")

// UpdateDBFromSwarmingTask updates a task in db from data in s.
func UpdateDBFromSwarmingTask(db TaskDB, s *swarming_api.SwarmingRpcsTaskResult) error {
	id, err := swarming.GetTagValue(s, SWARMING_TAG_ID)
	if err != nil {
		return err
	}
	_, err = UpdateTaskWithRetries(db, id, func(task *Task) error {
		modified, err := task.UpdateFromSwarming(s)
		if err != nil {
			return err
		}
		if !modified {
			return errNotModified
		}
		return nil
	})
	if err == errNotModified {
		return nil
	} else {
		return err
	}
}

func (t *Task) Done() bool {
	return t.Status != TASK_STATUS_PENDING && t.Status != TASK_STATUS_RUNNING
}

// Fake returns whether this Task does not correspond to a Swarming task.
func (t *Task) Fake() bool {
	return t.SwarmingTaskId == ""
}

func (t *Task) Success() bool {
	return t.Status == TASK_STATUS_SUCCESS
}

func (t *Task) Copy() *Task {
	return &Task{
		Attempt:        t.Attempt,
		Commits:        util.CopyStringSlice(t.Commits),
		Created:        t.Created,
		DbModified:     t.DbModified,
		Finished:       t.Finished,
		Id:             t.Id,
		IsolatedOutput: t.IsolatedOutput,
		Jobs:           util.CopyStringSlice(t.Jobs),
		MaxAttempts:    t.MaxAttempts,
		ParentTaskIds:  util.CopyStringSlice(t.ParentTaskIds),
		Properties:     util.CopyStringMap(t.Properties),
		RetryOf:        t.RetryOf,
		Started:        t.Started,
		Status:         t.Status,
		SwarmingBotId:  t.SwarmingBotId,
		SwarmingTaskId: t.SwarmingTaskId,
		TaskKey:        t.TaskKey.Copy(),
	}
}

// Validate returns an error if the task is not valid.
func (task *Task) Validate() error {
	if !task.TaskKey.Valid() {
		return fmt.Errorf("TaskKey is not valid.")
	}
	if task.Fake() && !(task.IsolatedOutput == "" && task.SwarmingBotId == "" && task.SwarmingTaskId == "") {
		return fmt.Errorf("Can not specify Swarming info for a fake task.")
	}
	for key, value := range task.Properties {
		if !utf8.ValidString(key) {
			return fmt.Errorf("Invalid property key -- must be valid UTF8: %q", key)
		}
		if !utf8.ValidString(value) {
			return fmt.Errorf("Invalid property value -- must be valid UTF8 or base64-encoded: %q", value)
		}
	}
	return nil
}

// Valid returns true if Validate() does not return an error. Hides
// task.TaskKey.Valid to prevent confusion.
func (task *Task) Valid() bool {
	return task.Validate() == nil
}

// TaskSummary is a subset of the information found in a Task.
type TaskSummary struct {
	Attempt        int        `json:"attempt"`
	Id             string     `json:"id"`
	MaxAttempts    int        `json:"max_attempts"`
	Status         TaskStatus `json:"status"`
	SwarmingTaskId string     `json:"swarmingTaskId"`
}

// MakeTaskSummary creates a TaskSummary from the Task instance.
func (t *Task) MakeTaskSummary() *TaskSummary {
	return &TaskSummary{
		Attempt:        t.Attempt,
		Id:             t.Id,
		MaxAttempts:    t.MaxAttempts,
		Status:         t.Status,
		SwarmingTaskId: t.SwarmingTaskId,
	}
}

// Copy returns a copy of the TaskSummary.
func (t *TaskSummary) Copy() *TaskSummary {
	return &TaskSummary{
		Attempt:        t.Attempt,
		Id:             t.Id,
		MaxAttempts:    t.MaxAttempts,
		Status:         t.Status,
		SwarmingTaskId: t.SwarmingTaskId,
	}
}

// TaskSlice implements sort.Interface. To sort tasks []*Task, use
// sort.Sort(TaskSlice(tasks)).
type TaskSlice []*Task

func (s TaskSlice) Len() int { return len(s) }

func (s TaskSlice) Less(i, j int) bool {
	return s[i].Created.Before(s[j].Created)
}

func (s TaskSlice) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

// TaskEncoder encodes Tasks into bytes via GOB encoding. Not safe for
// concurrent use.
// TODO(benjaminwagner): Encode in parallel.
type TaskEncoder struct {
	err    error
	tasks  []*Task
	result [][]byte
}

// Process encodes the Task into a byte slice that will be returned from Next()
// (in arbitrary order). Returns false if Next is certain to return an error.
// Caller must ensure t does not change until after the first call to Next().
// May not be called after calling Next().
func (e *TaskEncoder) Process(t *Task) bool {
	if e.err != nil {
		return false
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(t); err != nil {
		e.err = err
		e.tasks = nil
		e.result = nil
		return false
	}
	e.tasks = append(e.tasks, t)
	e.result = append(e.result, buf.Bytes())
	return true
}

// Next returns one of the Tasks provided to Process (in arbitrary order) and
// its serialized bytes. If any tasks remain, returns the task, the serialized
// bytes, nil. If all tasks have been returned, returns nil, nil, nil. If an
// error is encountered, returns nil, nil, error.
func (e *TaskEncoder) Next() (*Task, []byte, error) {
	if e.err != nil {
		return nil, nil, e.err
	}
	if len(e.tasks) == 0 {
		return nil, nil, nil
	}
	t := e.tasks[0]
	e.tasks = e.tasks[1:]
	serialized := e.result[0]
	e.result = e.result[1:]
	return t, serialized, nil
}

// TaskDecoder decodes bytes into Tasks via GOB decoding. Not safe for
// concurrent use.
type TaskDecoder struct {
	// input contains the incoming byte slices. Process() sends on this channel,
	// decode() receives from it, and Result() closes it.
	input chan []byte
	// output contains decoded Tasks. decode() sends on this channel, collect()
	// receives from it, and run() closes it when all decode() goroutines have
	// finished.
	output chan *Task
	// result contains the return value of Result(). collect() sends a single
	// value on this channel and closes it. Result() receives from it.
	result chan []*Task
	// errors contains the first error from any goroutine. It's a channel in case
	// multiple goroutines experience an error at the same time.
	errors chan error
}

const kNumDecoderGoroutines = 10

// init initializes d if it has not been initialized. May not be called concurrently.
func (d *TaskDecoder) init() {
	if d.input == nil {
		d.input = make(chan []byte, kNumDecoderGoroutines*2)
		d.output = make(chan *Task, kNumDecoderGoroutines)
		d.result = make(chan []*Task, 1)
		d.errors = make(chan error, kNumDecoderGoroutines)
		go d.run()
		go d.collect()
	}
}

// run starts the decode goroutines and closes d.output when they finish.
func (d *TaskDecoder) run() {
	// Start decoders.
	wg := sync.WaitGroup{}
	for i := 0; i < kNumDecoderGoroutines; i++ {
		wg.Add(1)
		go d.decode(&wg)
	}
	// Wait for decoders to exit.
	wg.Wait()
	// Drain d.input in the case that errors were encountered, to avoid deadlock.
	for range d.input {
	}
	close(d.output)
}

// decode receives from d.input and sends to d.output until d.input is closed or
// d.errors is non-empty. Decrements wg when done.
func (d *TaskDecoder) decode(wg *sync.WaitGroup) {
	for b := range d.input {
		var t Task
		if err := gob.NewDecoder(bytes.NewReader(b)).Decode(&t); err != nil {
			d.errors <- err
			break
		}
		d.output <- &t
		if len(d.errors) > 0 {
			break
		}
	}
	wg.Done()
}

// collect receives from d.output until it is closed, then sends on d.result.
func (d *TaskDecoder) collect() {
	result := []*Task{}
	for t := range d.output {
		result = append(result, t)
	}
	d.result <- result
	close(d.result)
}

// Process decodes the byte slice into a Task and includes it in Result() (in
// arbitrary order). Returns false if Result is certain to return an error.
// Caller must ensure b does not change until after Result() returns.
func (d *TaskDecoder) Process(b []byte) bool {
	d.init()
	d.input <- b
	return len(d.errors) == 0
}

// Result returns all decoded Tasks provided to Process (in arbitrary order), or
// any error encountered.
func (d *TaskDecoder) Result() ([]*Task, error) {
	// Allow TaskDecoder to be used without initialization.
	if d.result == nil {
		return []*Task{}, nil
	}
	close(d.input)
	select {
	case err := <-d.errors:
		return nil, err
	case result := <-d.result:
		return result, nil
	}
}

// TagsForTask returns the tags which should be set for a Task.
func TagsForTask(name, id string, attempt int, priority float64, rs RepoState, retryOf string, dimensions map[string]string, forcedJobId string, parentTaskIds []string) []string {
	tags := map[string]string{
		SWARMING_TAG_ATTEMPT:         fmt.Sprintf("%d", attempt),
		SWARMING_TAG_FORCED_JOB_ID:   forcedJobId,
		SWARMING_TAG_NAME:            name,
		SWARMING_TAG_ID:              id,
		SWARMING_TAG_LUCI_PROJECT:    common.REPO_PROJECT_MAPPING[rs.Repo],
		SWARMING_TAG_PRIORITY:        fmt.Sprintf("%f", priority),
		SWARMING_TAG_REPO:            rs.Repo,
		SWARMING_TAG_RETRY_OF:        retryOf,
		SWARMING_TAG_REVISION:        rs.Revision,
		SWARMING_TAG_SOURCE_REVISION: rs.Revision,
		SWARMING_TAG_SOURCE_REPO:     rs.Repo + "/+/%s",
	}
	if rs.IsTryJob() {
		tags[SWARMING_TAG_SERVER] = rs.Server
		tags[SWARMING_TAG_ISSUE] = rs.Issue
		tags[SWARMING_TAG_PATCHSET] = rs.Patchset
	}

	for k, v := range dimensions {
		key := fmt.Sprintf("%s%s", SWARMING_TAG_DIMENSION_PREFIX, k)
		if _, ok := tags[key]; !ok {
			tags[key] = v
		} else {
			sklog.Warningf("Duplicate dimension/tag %q.", k)
		}
	}

	tagsList := make([]string, 0, len(tags)+len(parentTaskIds))
	for k, v := range tags {
		tagsList = append(tagsList, fmt.Sprintf("%s:%s", k, v))
	}
	for _, id := range parentTaskIds {
		tagsList = append(tagsList, fmt.Sprintf("%s:%s", SWARMING_TAG_PARENT_TASK_ID, id))
	}
	return tagsList
}

// DimensionsFromTags returns a set of dimensions based on the given tags.
func DimensionsFromTags(tags []string) []string {
	rv := make([]string, 0, len(tags))
	for _, t := range tags {
		if strings.HasPrefix(t, SWARMING_TAG_DIMENSION_PREFIX) {
			rv = append(rv, t[len(SWARMING_TAG_DIMENSION_PREFIX):])
		}
	}
	return rv
}
