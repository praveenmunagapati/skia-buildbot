package main

/*
	Performance test for TaskScheduler.
*/

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math"
	"net/http"
	_ "net/http/pprof"
	"os"
	"path"
	"reflect"
	"time"

	"github.com/davecgh/go-spew/spew"
	swarming_api "github.com/luci/luci-go/common/api/swarming/swarming/v1"
	"github.com/skia-dev/glog"
	"go.skia.org/infra/build_scheduler/go/db"
	"go.skia.org/infra/build_scheduler/go/task_scheduler"
	"go.skia.org/infra/go/common"
	"go.skia.org/infra/go/exec"
	"go.skia.org/infra/go/gitinfo"
	"go.skia.org/infra/go/isolate"
	"go.skia.org/infra/go/swarming"
	"go.skia.org/infra/go/timer"
)

func assertNoError(err error) {
	if err != nil {
		glog.Fatalf("Expected no error but got: %s", err.Error())
	}
}

func assertEqual(a, b interface{}) {
	if a != b {
		glog.Fatalf("Expected %v but got %v", a, b)
	}
}

func assertDeepEqual(a, b interface{}) {
	if !reflect.DeepEqual(a, b) {
		glog.Fatalf("Objects do not match: \na:\n%s\n\nb:\n%s\n", spew.Sprint(a), spew.Sprint(b))
	}
}

func makeBot(id string, dims map[string]string) *swarming_api.SwarmingRpcsBotInfo {
	dimensions := make([]*swarming_api.SwarmingRpcsStringListPair, 0, len(dims))
	for k, v := range dims {
		dimensions = append(dimensions, &swarming_api.SwarmingRpcsStringListPair{
			Key:   k,
			Value: []string{v},
		})
	}
	return &swarming_api.SwarmingRpcsBotInfo{
		BotId:      id,
		Dimensions: dimensions,
	}
}

func makeDummyCommits(repoDir string, numCommits int) {
	_, err := exec.RunCwd(repoDir, "git", "checkout", "master")
	assertNoError(err)
	dummyFile := path.Join(repoDir, "dummyfile.txt")
	for i := 0; i < numCommits; i++ {
		title := fmt.Sprintf("Dummy #%d", i)
		assertNoError(ioutil.WriteFile(dummyFile, []byte(title), os.ModePerm))
		_, err = exec.RunCwd(repoDir, "git", "add", dummyFile)
		assertNoError(err)
		_, err = exec.RunCwd(repoDir, "git", "commit", "-m", title)
		assertNoError(err)
		_, err = exec.RunCwd(repoDir, "git", "push", "origin", "master")
		assertNoError(err)
	}
}

func run(dir string, cmd ...string) {
	if _, err := exec.RunCwd(dir, cmd...); err != nil {
		glog.Fatal(err)
	}
}

func addFile(repoDir, subPath, contents string) {
	assertNoError(ioutil.WriteFile(path.Join(repoDir, subPath), []byte(contents), os.ModePerm))
	run(repoDir, "git", "add", subPath)
}

func main() {
	common.Init()
	defer common.LogPanic()

	// Create a repo with lots of commits.
	workdir, err := ioutil.TempDir("", "")
	assertNoError(err)
	defer func() {
		//if err := os.RemoveAll(workdir); err != nil {
		//	glog.Fatal(err)
		//}
	}()
	d := db.NewInMemoryDB()
	cache, err := db.NewTaskCache(d, time.Hour)
	assertNoError(err)

	repoName := "skia.git"
	repoDir := path.Join(workdir, repoName)
	assertNoError(os.Mkdir(path.Join(workdir, repoName), os.ModePerm))
	run(repoDir, "git", "init")
	run(repoDir, "git", "remote", "add", "origin", ".")

	repos := gitinfo.NewRepoMap(workdir)
	assertNoError(err)

	// Write some files.
	assertNoError(ioutil.WriteFile(path.Join(workdir, ".gclient"), []byte("dummy"), os.ModePerm))
	addFile(repoDir, "a.txt", "dummy2")
	addFile(repoDir, "somefile.txt", "dummy3")
	infraBotsSubDir := path.Join("infra", "bots")
	infraBotsDir := path.Join(repoDir, infraBotsSubDir)
	assertNoError(os.MkdirAll(infraBotsDir, os.ModePerm))
	addFile(repoDir, path.Join(infraBotsSubDir, "compile_skia.isolate"), `{
  'includes': [
    'swarm_recipe.isolate',
  ],
  'variables': {
    'files': [
      '../../../.gclient',
    ],
  },
}`)
	addFile(repoDir, path.Join(infraBotsSubDir, "perf_skia.isolate"), `{
  'includes': [
    'swarm_recipe.isolate',
  ],
  'variables': {
    'files': [
      '../../../.gclient',
    ],
  },
}`)
	addFile(repoDir, path.Join(infraBotsSubDir, "test_skia.isolate"), `{
  'includes': [
    'swarm_recipe.isolate',
  ],
  'variables': {
    'files': [
      '../../../.gclient',
    ],
  },
}`)
	addFile(repoDir, path.Join(infraBotsSubDir, "swarm_recipe.isolate"), `{
  'variables': {
    'command': [
      'python', 'recipes.py', 'run',
    ],
    'files': [
      '../../somefile.txt',
    ],
  },
}`)

	// Add tasks to the repo.
	var tasks = map[string]*task_scheduler.TaskSpec{
		"Build-Ubuntu-GCC-Arm7-Release-Android": &task_scheduler.TaskSpec{
			CipdPackages: []*task_scheduler.CipdPackage{},
			Dependencies: []string{},
			Dimensions:   []string{"pool:Skia", "os:Ubuntu"},
			Isolate:      "compile_skia.isolate",
			Priority:     0.9,
		},
		"Test-Android-GCC-Nexus7-GPU-Tegra3-Arm7-Release": &task_scheduler.TaskSpec{
			CipdPackages: []*task_scheduler.CipdPackage{},
			Dependencies: []string{"Build-Ubuntu-GCC-Arm7-Release-Android"},
			Dimensions:   []string{"pool:Skia", "os:Android", "device_type:grouper"},
			Isolate:      "test_skia.isolate",
			Priority:     0.9,
		},
		"Perf-Android-GCC-Nexus7-GPU-Tegra3-Arm7-Release": &task_scheduler.TaskSpec{
			CipdPackages: []*task_scheduler.CipdPackage{},
			Dependencies: []string{"Build-Ubuntu-GCC-Arm7-Release-Android"},
			Dimensions:   []string{"pool:Skia", "os:Android", "device_type:grouper"},
			Isolate:      "perf_skia.isolate",
			Priority:     0.9,
		},
	}
	moarTasks := map[string]*task_scheduler.TaskSpec{}
	for name, task := range tasks {
		for i := 0; i < 100; i++ {
			newName := fmt.Sprintf("%s%d", name, i)
			deps := make([]string, 0, len(task.Dependencies))
			for _, d := range task.Dependencies {
				deps = append(deps, fmt.Sprintf("%s%d", d, i))
			}
			newTask := &task_scheduler.TaskSpec{
				CipdPackages: task.CipdPackages,
				Dependencies: deps,
				Dimensions:   task.Dimensions,
				Isolate:      task.Isolate,
				Priority:     task.Priority,
			}
			moarTasks[newName] = newTask
		}
	}
	cfg := task_scheduler.TasksCfg{
		Tasks: moarTasks,
	}
	f, err := os.Create(path.Join(repoDir, task_scheduler.TASKS_CFG_FILE))
	assertNoError(err)
	assertNoError(json.NewEncoder(f).Encode(&cfg))
	assertNoError(f.Close())
	run(repoDir, "git", "add", task_scheduler.TASKS_CFG_FILE)
	run(repoDir, "git", "commit", "-m", "Add more tasks!")
	run(repoDir, "git", "push", "origin", "master")
	run(repoDir, "git", "branch", "-u", "origin/master")

	// Create a bunch of bots.
	bots := make([]*swarming_api.SwarmingRpcsBotInfo, 100)
	for idx, _ := range bots {
		dims := map[string]string{
			"pool": "Skia",
		}
		if idx > 50 {
			dims["os"] = "Ubuntu"
		} else {
			dims["os"] = "Android"
			dims["device_type"] = "grouper"
		}
		bots[idx] = makeBot(fmt.Sprintf("bot%d", idx), dims)
	}

	// Create the task scheduler.
	repo, err := repos.Repo(repoName)
	assertNoError(err)
	head, err := repo.FullHash("HEAD")
	assertNoError(err)

	commits, err := repo.RevList(head)
	assertNoError(err)
	assertDeepEqual([]string{head}, commits)

	isolateClient, err := isolate.NewClient(workdir)
	assertNoError(err)
	isolateClient.ServerUrl = isolate.FAKE_SERVER_URL
	swarmingClient := swarming.NewTestClient()
	s, err := task_scheduler.NewTaskScheduler(d, cache, time.Duration(math.MaxInt64), workdir, []string{"skia.git"}, isolateClient, swarmingClient)
	assertNoError(err)

	runTasks := func(bots []*swarming_api.SwarmingRpcsBotInfo) {
		defer timer.New("cycle").Stop()
		swarmingClient.MockBots(bots)
		assertNoError(s.MainLoop())
		tasks, err := cache.GetTasksForCommits(commits)
		assertNoError(err)
		newTasks := map[string]*db.Task{}
		for _, v := range tasks {
			for _, task := range v {
				if task.Status == db.TASK_STATUS_PENDING {
					if _, ok := newTasks[task.Id]; !ok {
						newTasks[task.Id] = task
					}
				}
			}
		}
		insert := make([]*db.Task, 0, len(newTasks))
		for _, task := range newTasks {
			task.Status = db.TASK_STATUS_SUCCESS
			task.Finished = time.Now()
			task.IsolatedOutput = "abc123"
			insert = append(insert, task)
		}
		assertNoError(d.PutTasks(insert))
		assertNoError(cache.Update())
	}

	// Consume all tasks.
	for {
		runTasks(bots)
		tasks, err := cache.GetTasksForCommits(commits)
		assertNoError(err)
		if s.QueueLen() == 0 {
			assertEqual(len(moarTasks), len(tasks[head]))
			break
		}
	}

	// Add more commits to the repo.
	makeDummyCommits(repoDir, 200)

	go func() {
		glog.Fatal(http.ListenAndServe("localhost:6060", nil))
	}()

	// Actually run the test.
	i := 0
	for ; ; i++ {
		runTasks(bots)
		if s.QueueLen() == 0 {
			break
		}
	}
	glog.Infof("Finished in %d iterations.", i)
}