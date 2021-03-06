// run_chromium_analysis_on_workers is an application that runs the specified
// telemetry benchmark on swarming bots and uploads the results to Google
// Storage. The requester is emailed when the task is done.
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.skia.org/infra/ct/go/ctfe/chromium_analysis"
	"go.skia.org/infra/ct/go/frontend"
	"go.skia.org/infra/ct/go/master_scripts/master_common"
	"go.skia.org/infra/ct/go/util"
	"go.skia.org/infra/go/common"
	"go.skia.org/infra/go/email"
	"go.skia.org/infra/go/sklog"
	skutil "go.skia.org/infra/go/util"
)

const (
	MAX_PAGES_PER_SWARMING_BOT = 100
)

var (
	emails             = flag.String("emails", "", "The comma separated email addresses to notify when the task is picked up and completes.")
	description        = flag.String("description", "", "The description of the run as entered by the requester.")
	gaeTaskID          = flag.Int64("gae_task_id", -1, "The key of the task. This task will be updated when the task is started and completed.")
	pagesetType        = flag.String("pageset_type", "", "The type of pagesets to use. Eg: 10k, Mobile10k, All.")
	benchmarkName      = flag.String("benchmark_name", "", "The telemetry benchmark to run on the workers.")
	benchmarkExtraArgs = flag.String("benchmark_extra_args", "", "The extra arguments that are passed to the specified benchmark.")
	browserExtraArgs   = flag.String("browser_extra_args", "", "The extra arguments that are passed to the browser while running the benchmark.")
	runID              = flag.String("run_id", "", "The unique run id (typically requester + timestamp).")
	targetPlatform     = flag.String("target_platform", util.PLATFORM_LINUX, "The platform the benchmark will run on (Android / Linux).")
	runOnGCE           = flag.Bool("run_on_gce", true, "Run on Linux GCE instances. Used only if Linux is used for the target_platform.")
	runInParallel      = flag.Bool("run_in_parallel", true, "Run the benchmark by bringing up multiple chrome instances in parallel.")

	taskCompletedSuccessfully = false

	chromiumPatchLink  = util.MASTER_LOGSERVER_LINK
	v8PatchLink        = util.MASTER_LOGSERVER_LINK
	catapultPatchLink  = util.MASTER_LOGSERVER_LINK
	benchmarkPatchLink = util.MASTER_LOGSERVER_LINK
	customWebpagesLink = util.MASTER_LOGSERVER_LINK
	outputLink         = util.MASTER_LOGSERVER_LINK
)

func sendEmail(recipients []string, gs *util.GcsUtil) {
	// Send completion email.
	emailSubject := fmt.Sprintf("Cluster telemetry chromium analysis task has completed (%s)", *runID)
	failureHtml := ""
	viewActionMarkup := ""
	var err error

	if taskCompletedSuccessfully {
		if viewActionMarkup, err = email.GetViewActionMarkup(outputLink, "View Results", "Direct link to the CSV results"); err != nil {
			sklog.Errorf("Failed to get view action markup: %s", err)
			return
		}
	} else {
		emailSubject += " with failures"
		failureHtml = util.GetFailureEmailHtml(*runID)
		if viewActionMarkup, err = email.GetViewActionMarkup(fmt.Sprintf(util.SWARMING_RUN_ID_ALL_TASKS_LINK_TEMPLATE, *runID), "View Failure", "Direct link to the swarming logs"); err != nil {
			sklog.Errorf("Failed to get view action markup: %s", err)
			return
		}
	}

	totalArchivedWebpages, err := util.GetArchivesNum(gs, *benchmarkExtraArgs, *pagesetType)
	if err != nil {
		sklog.Errorf("Error when calculating number of archives: %s", err)
		totalArchivedWebpages = -1
	}
	archivedWebpagesText := ""
	if totalArchivedWebpages != -1 {
		archivedWebpagesText = fmt.Sprintf(" %d WPR archives were used.", totalArchivedWebpages)
	}

	bodyTemplate := `
	The chromium analysis %s benchmark task on %s pageset has completed. %s.<br/>
	Run description: %s<br/>
	%s
	The CSV output is <a href='%s'>here</a>.%s<br/>
	The patch(es) you specified are here:
	<a href='%s'>chromium</a>/<a href='%s'>v8</a>/<a href='%s'>catapult</a>/<a href='%s'>telemetry</a>
	<br/>
	Custom webpages (if specified) are <a href='%s'>here</a>.
	<br/><br/>
	You can schedule more runs <a href='%s'>here</a>.
	<br/><br/>
	Thanks!
	`
	emailBody := fmt.Sprintf(bodyTemplate, *benchmarkName, *pagesetType, util.GetSwarmingLogsLink(*runID), *description, failureHtml, outputLink, archivedWebpagesText, chromiumPatchLink, v8PatchLink, catapultPatchLink, benchmarkPatchLink, customWebpagesLink, frontend.ChromiumAnalysisTasksWebapp)
	if err := util.SendEmailWithMarkup(recipients, emailSubject, emailBody, viewActionMarkup); err != nil {
		sklog.Errorf("Error while sending email: %s", err)
		return
	}
}

func updateWebappTask() {
	vars := chromium_analysis.UpdateVars{}
	vars.Id = *gaeTaskID
	vars.SetCompleted(taskCompletedSuccessfully)
	vars.RawOutput = sql.NullString{String: outputLink, Valid: true}
	skutil.LogErr(frontend.UpdateWebappTaskV2(&vars))
}

func main() {
	defer common.LogPanic()
	master_common.Init("run_chromium_analysis")

	// Send start email.
	emailsArr := util.ParseEmails(*emails)
	emailsArr = append(emailsArr, util.CtAdmins...)
	if len(emailsArr) == 0 {
		sklog.Error("At least one email address must be specified")
		return
	}
	// Instantiate GcsUtil object.
	gs, err := util.NewGcsUtil(nil)
	if err != nil {
		sklog.Errorf("Could not instantiate gsutil object: %s", err)
		return
	}

	skutil.LogErr(frontend.UpdateWebappTaskSetStarted(&chromium_analysis.UpdateVars{}, *gaeTaskID, *runID))
	skutil.LogErr(util.SendTaskStartEmail(emailsArr, "Chromium analysis", *runID, *description))
	// Ensure webapp is updated and email is sent even if task fails.
	defer updateWebappTask()
	defer sendEmail(emailsArr, gs)
	// Cleanup dirs after run completes.
	defer skutil.RemoveAll(filepath.Join(util.StorageDir, util.BenchmarkRunsDir, *runID))
	// Finish with glog flush and how long the task took.
	defer util.TimeTrack(time.Now(), "Running chromium analysis task on workers")
	defer sklog.Flush()

	if *pagesetType == "" {
		sklog.Error("Must specify --pageset_type")
		return
	}
	if *benchmarkName == "" {
		sklog.Error("Must specify --benchmark_name")
		return
	}
	if *runID == "" {
		sklog.Error("Must specify --run_id")
		return
	}

	remoteOutputDir := filepath.Join(util.ChromiumAnalysisRunsDir, *runID)

	// Copy the patches and custom webpages to Google Storage.
	chromiumPatchName := *runID + ".chromium.patch"
	v8PatchName := *runID + ".v8.patch"
	catapultPatchName := *runID + ".catapult.patch"
	benchmarkPatchName := *runID + ".benchmark.patch"
	customWebpagesName := *runID + ".custom_webpages.csv"
	for _, patchName := range []string{chromiumPatchName, v8PatchName, catapultPatchName, benchmarkPatchName, customWebpagesName} {
		if err := gs.UploadFile(patchName, os.TempDir(), remoteOutputDir); err != nil {
			sklog.Errorf("Could not upload %s to %s: %s", patchName, remoteOutputDir, err)
			return
		}
	}
	chromiumPatchLink = util.GCS_HTTP_LINK + filepath.Join(util.GCSBucketName, remoteOutputDir, chromiumPatchName)
	v8PatchLink = util.GCS_HTTP_LINK + filepath.Join(util.GCSBucketName, remoteOutputDir, v8PatchName)
	catapultPatchLink = util.GCS_HTTP_LINK + filepath.Join(util.GCSBucketName, remoteOutputDir, catapultPatchName)
	benchmarkPatchLink = util.GCS_HTTP_LINK + filepath.Join(util.GCSBucketName, remoteOutputDir, benchmarkPatchName)
	customWebpagesLink = util.GCS_HTTP_LINK + filepath.Join(util.GCSBucketName, remoteOutputDir, customWebpagesName)

	// Create the required chromium build.
	chromiumBuilds, err := util.TriggerBuildRepoSwarmingTask("build_chromium", *runID, "chromium", *targetPlatform, []string{}, []string{filepath.Join(remoteOutputDir, chromiumPatchName), filepath.Join(remoteOutputDir, v8PatchName)}, true /*singleBuild*/, 3*time.Hour, 1*time.Hour)
	if err != nil {
		sklog.Errorf("Error encountered when swarming build repo task: %s", err)
		return
	}
	if len(chromiumBuilds) != 1 {
		sklog.Errorf("Expected 1 build but instead got %d: %v", len(chromiumBuilds), chromiumBuilds)
		return
	}
	chromiumBuild := chromiumBuilds[0]

	// Archive, trigger and collect swarming tasks.
	isolateExtraArgs := map[string]string{
		"CHROMIUM_BUILD":     chromiumBuild,
		"RUN_ID":             *runID,
		"BENCHMARK":          *benchmarkName,
		"BENCHMARK_ARGS":     *benchmarkExtraArgs,
		"BROWSER_EXTRA_ARGS": *browserExtraArgs,
		"RUN_IN_PARALLEL":    strconv.FormatBool(*runInParallel),
		"TARGET_PLATFORM":    *targetPlatform,
	}

	// Figure out priority of analysis swarming tasks.
	priority := util.USER_TASKS_PRIORITY
	if *pagesetType == util.PAGESET_TYPE_SVG_5m {
		// Run long running tasks like SVG5M at a lower priority to avoid starving other user triggered tasks.
		priority = util.LONG_RUNNING_TASKS_PRIORITY
	}

	customWebPagesFilePath := filepath.Join(os.TempDir(), customWebpagesName)
	numPages, err := util.GetNumPages(*pagesetType, customWebPagesFilePath)
	if err != nil {
		sklog.Errorf("Error encountered when calculating number of pages: %s", err)
		return
	}
	// Calculate the max pages to run per bot.
	maxPagesPerBot := util.GetMaxPagesPerBotValue(*benchmarkExtraArgs, MAX_PAGES_PER_SWARMING_BOT)
	numSlaves, err := util.TriggerSwarmingTask(*pagesetType, "chromium_analysis", util.CHROMIUM_ANALYSIS_ISOLATE, *runID, 12*time.Hour, 1*time.Hour, priority, maxPagesPerBot, numPages, isolateExtraArgs, *runOnGCE, util.GetRepeatValue(*benchmarkExtraArgs, 1))
	if err != nil {
		sklog.Errorf("Error encountered when swarming tasks: %s", err)
		return
	}

	// If "--output-format=csv" is specified then merge all CSV files and upload.
	noOutputSlaves := []string{}
	pathToPyFiles := util.GetPathToPyFiles(false)
	if strings.Contains(*benchmarkExtraArgs, "--output-format=csv") {
		if noOutputSlaves, err = util.MergeUploadCSVFiles(*runID, pathToPyFiles, gs, numPages, maxPagesPerBot, true /* handleStrings */, util.GetRepeatValue(*benchmarkExtraArgs, 1)); err != nil {
			sklog.Errorf("Unable to merge and upload CSV files for %s: %s", *runID, err)
		}
		// Cleanup created dir after the run completes.
		defer skutil.RemoveAll(filepath.Join(util.StorageDir, util.BenchmarkRunsDir, *runID))
	}
	// If the number of noOutputSlaves is the same as the total number of triggered slaves then consider the run failed.
	if len(noOutputSlaves) == numSlaves {
		sklog.Errorf("All %d slaves produced no output", numSlaves)
		return
	}

	// Construct the output link.
	outputLink = util.GCS_HTTP_LINK + filepath.Join(util.GCSBucketName, util.BenchmarkRunsDir, *runID, "consolidated_outputs", *runID+".output")

	// Display the no output slaves.
	for _, noOutputSlave := range noOutputSlaves {
		directLink := fmt.Sprintf(util.SWARMING_RUN_ID_TASK_LINK_PREFIX_TEMPLATE, *runID, "chromium_analysis_"+noOutputSlave)
		fmt.Printf("Missing output from %s\n", directLink)
	}

	taskCompletedSuccessfully = true
}
