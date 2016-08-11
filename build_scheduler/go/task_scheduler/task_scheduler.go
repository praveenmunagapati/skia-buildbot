package task_scheduler

import (
	"fmt"
	"sort"

	"github.com/skia-dev/glog"
	"go.skia.org/infra/build_scheduler/go/db"
	"go.skia.org/infra/go/buildbot"
	"go.skia.org/infra/go/gitinfo"
)

// TaskScheduler is a struct used for scheduling builds on bots.
type TaskScheduler struct {
	cache        *db.TaskCache
	taskCfgCache *taskCfgCache
}

func NewTaskScheduler(cache *db.TaskCache, workdir string) *TaskScheduler {
	repos := gitinfo.NewRepoMap(workdir)
	return &TaskScheduler{
		cache:        cache,
		taskCfgCache: newTaskCfgCache(repos),
	}
}

type taskCandidate struct {
	Commits        []string
	IsolatedHashes []string
	Name           string
	Repo           string
	Revision       string
	Score          float64
	TaskSpec       *TaskSpec
}

// MakeTask instantiates a db.Task from the taskCandidate.
func (c *taskCandidate) MakeTask() *db.Task {
	commits := make([]string, 0, len(c.Commits))
	copy(commits, c.Commits)
	return &db.Task{
		SwarmingRpcsTaskRequestMetadata: nil,
		Commits:        commits,
		Id:             "", // Filled in when the task is inserted into the DB.
		IsolatedOutput: "", // Filled in when the task finishes, if successful.
		Name:           c.Name,
		Repo:           c.Repo,
		Revision:       c.Revision,
	}
}

// AllDepsMet determines whether all dependencies for the given task candidate
// have been satisfied, and if so, returns their isolated outputs.
func (s *TaskScheduler) AllDepsMet(c *taskCandidate) (bool, []string, error) {
	isolatedHashes := make([]string, 0, len(c.TaskSpec.Dependencies))
	for _, depName := range c.TaskSpec.Dependencies {
		d, err := s.cache.GetTaskForCommit(depName, c.Revision)
		if err != nil {
			return false, nil, err
		}
		if d == nil {
			return false, nil, nil
		}
		if !d.Finished() || !d.Success() || d.IsolatedOutput == "" {
			return false, nil, nil
		}
		isolatedHashes = append(isolatedHashes, d.IsolatedOutput)
	}
	return true, isolatedHashes, nil
}

// ComputeBlamelist computes the blamelist for the given taskCandidate. Returns
// the list of commits covered by the task, and any previous task which part or
// all of the blamelist was "stolen" from (see below). There are three cases:
//
// 1. This taskCandidate tests commits which have not yet been tested. Trace
//    commit history, accumulating commits until we find commits which have
//    been tested by previous tasks.
//
// 2. This taskCandidate runs at the same commit as a previous task. This is a
//    retry, so the entire blamelist of the previous task is "stolen".
//
// 3. This taskCandidate runs at a commit which is in a previous task's
//    blamelist, but no task has run at the same commit. This is a bisect. Trace
//    commit history, "stealing" commits from the previous task until we find a
//    commit which was covered by a *different* previous task.
func ComputeBlamelist(cache *db.TaskCache, repos *gitinfo.RepoMap, c *taskCandidate) ([]string, *db.Task, error) {
	commits := map[string]bool{}
	var stealFrom *db.Task

	// TODO(borenet): If this is a try job, don't compute the blamelist.

	// If this is the first invocation of a given task spec, save time by
	// setting the blamelist to only be c.Revision.
	if !cache.KnownTaskName(c.Name) {
		return []string{c.Revision}, nil, nil
	}

	repo, err := repos.Repo(c.Repo)
	if err != nil {
		return nil, nil, fmt.Errorf("Could not compute blamelist for task candidate: %s", err)
	}

	// computeBlamelistRecursive traces through commit history, adding to
	// the commits map until the blamelist for the task is complete.
	var computeBlamelistRecursive func(string) error
	computeBlamelistRecursive = func(hash string) error {
		// Shortcut for empty hashes. This can happen when a commit has
		// no parents.
		if hash == "" {
			return nil
		}

		// Shortcut in case we missed this case before; if this is the first
		// task for this task spec which has a valid Revision, the blamelist will
		// be the entire Git history. If we find too many commits, assume we've
		// hit this case and just return the Revision as the blamelist.
		if len(commits) > buildbot.MAX_BLAMELIST_COMMITS && stealFrom == nil {
			commits = map[string]bool{
				c.Revision: true,
			}
			return nil
		}

		// Determine whether any task already includes this commit.
		prev, err := cache.GetTaskForCommit(c.Name, hash)
		if err != nil {
			return fmt.Errorf("Could not find task %q for commit %q: %s", c.Name, hash, err)
		}

		// If we're stealing commits from a previous task but the current
		// commit is not in any task's blamelist, we must have scrolled past
		// the beginning of the tasks. Just return.
		if prev == nil && stealFrom != nil {
			return nil
		}

		// If a previous task already included this commit, we have to make a decision.
		if prev != nil {
			// If this Task's Revision is already included in a different
			// Task, then we're either bisecting or retrying a task. We'll
			// "steal" commits from the previous Task's blamelist.
			if len(commits) == 0 {
				stealFrom = prev

				// Another shortcut: If our Revision is the same as the
				// Revision of the Task we're stealing commits from,
				// ie. both tasks ran at the same commit, then this is a
				// retry. Just steal all of the commits without doing
				// any more work.
				if stealFrom.Revision == c.Revision {
					commits = map[string]bool{}
					for _, c := range stealFrom.Commits {
						commits[c] = true
					}
					return nil
				}
			}
			if stealFrom == nil || prev.Id != stealFrom.Id {
				// If we've hit a commit belonging to a different task,
				// we're done.
				return nil
			}
		}

		// Add the commit.
		commits[hash] = true

		// Recurse on the commit's parents.
		details, err := repo.Details(hash, false)
		if err != nil {
			return err
		}
		for _, p := range details.Parents {
			// If we've already seen this parent commit, don't revisit it.
			if _, ok := commits[p]; ok {
				continue
			}
			if err := computeBlamelistRecursive(p); err != nil {
				return err
			}
		}
		return nil
	}

	// Run the helper function to recurse on commit history.
	if err := computeBlamelistRecursive(c.Revision); err != nil {
		return nil, nil, err
	}

	rv := make([]string, 0, len(commits))
	for c, _ := range commits {
		rv = append(rv, c)
	}
	sort.Strings(rv)
	return rv, stealFrom, nil
}

func (s *TaskScheduler) FindTaskCandidates(commitsByRepo map[string][]string) ([]*taskCandidate, error) {
	// Obtain all possible tasks.
	specs, err := s.taskCfgCache.GetTaskSpecsForCommits(commitsByRepo)
	if err != nil {
		return nil, err
	}
	candidates := []*taskCandidate{}
	for repo, commits := range specs {
		for commit, tasks := range commits {
			for name, task := range tasks {
				// We shouldn't duplicate pending, in-progress,
				// or successfully completed tasks.
				previous, err := s.cache.GetTaskForCommit(name, commit)
				if err != nil {
					return nil, err
				}
				if previous != nil {
					if previous.TaskResult.State == db.TASK_STATE_PENDING || previous.TaskResult.State == db.TASK_STATE_RUNNING {
						continue
					}
					if previous.Success() {
						continue
					}
				}
				candidates = append(candidates, &taskCandidate{
					IsolatedHashes: nil,
					Name:           name,
					Repo:           repo,
					Revision:       commit,
					Score:          0.0,
					TaskSpec:       task,
				})
			}
		}
	}

	// Filter out candidates whose dependencies have not been met.
	validCandidates := make([]*taskCandidate, 0, len(candidates))
	for _, c := range candidates {
		depsMet, hashes, err := s.AllDepsMet(c)
		if err != nil {
			return nil, err
		}
		if !depsMet {
			continue
		}
		c.IsolatedHashes = hashes
		validCandidates = append(validCandidates, c)
	}

	return validCandidates, nil
}

// testedness computes the total "testedness" of a set of commits covered by a
// task whose blamelist included N commits. The "testedness" of a task spec at a
// given commit is defined as follows:
//
// -1.0    if no task has ever included this commit for this task spec.
// 1.0     if a task was run for this task spec AT this commit.
// 1.0 / N if a task for this task spec has included this commit, where N is
//         the number of commits included in the task.
//
// This function gives the sum of the testedness for a blamelist of N commits.
func testedness(n int) float64 {
	if n < 0 {
		// This should never happen.
		glog.Errorf("Task score function got a blamelist with %d commits", n)
		return -1.0
	} else if n == 0 {
		// Zero commits have zero testedness.
		return 0.0
	} else if n == 1 {
		return 1.0
	} else {
		return 1.0 + float64(n-1)/float64(n)
	}
}

// testednessIncrease computes the increase in "testedness" obtained by running
// a task with the given blamelist length which may have "stolen" commits from
// a previous task with a different blamelist length. To do so, we compute the
// "testedness" for every commit affected by the task,  before and after the
// task would run. We subtract the "before" score from the "after" score to
// obtain the "testedness" increase at each commit, then sum them to find the
// total increase in "testedness" obtained by running the task.
func testednessIncrease(blamelistLength, stoleFromBlamelistLength int) float64 {
	// Invalid inputs.
	if blamelistLength <= 0 || stoleFromBlamelistLength < 0 {
		return -1.0
	}

	if stoleFromBlamelistLength == 0 {
		// This task covers previously-untested commits. Previous testedness
		// is -1.0 for each commit in the blamelist.
		beforeTestedness := float64(-blamelistLength)
		afterTestedness := testedness(blamelistLength)
		return afterTestedness - beforeTestedness
	} else if blamelistLength == stoleFromBlamelistLength {
		// This is a retry. It provides no testedness increase, so shortcut here
		// rather than perform the math to obtain the same answer.
		return 0.0
	} else {
		// This is a bisect/backfill.
		beforeTestedness := testedness(stoleFromBlamelistLength)
		afterTestedness := testedness(blamelistLength) + testedness(stoleFromBlamelistLength-blamelistLength)
		return afterTestedness - beforeTestedness
	}
}
