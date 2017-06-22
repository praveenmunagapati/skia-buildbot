// pixel_diff_on_workers is an application that captures screenshots of the
// specified patchset type on all CT workers and uploads the results to Google
// Storage. The requester is emailed when the task is done.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.skia.org/infra/ct/go/ctfe/capture_skps"
	"go.skia.org/infra/ct/go/frontend"
	"go.skia.org/infra/ct/go/master_scripts/master_common"
	"go.skia.org/infra/ct/go/util"
	"go.skia.org/infra/go/common"
	"go.skia.org/infra/go/email"
	"go.skia.org/infra/go/sklog"
	skutil "go.skia.org/infra/go/util"
)

const (
	MAX_PAGES_PER_SWARMING_BOT = 50
)

var (
	emails                    = flag.String("emails", "", "The comma separated email addresses to notify when the task is picked up and completes.")
	description               = flag.String("description", "", "The description of the run as entered by the requester.")
	gaeTaskID                 = flag.Int64("gae_task_id", -1, "The key of the App Engine task. This task will be updated when the task is completed.")
	pagesetType               = flag.String("pageset_type", "", "The type of pagesets to use. Eg: 10k, Mobile10k, All.")
	benchmarkExtraArgs        = flag.String("benchmark_extra_args", "", "The extra arguments that are passed to the specified benchmark.")
	browserExtraArgsNoPatch   = flag.String("browser_extra_args_nopatch", "", "The extra arguments that are passed to the browser while running the benchmark for the nopatch case.")
	browserExtraArgsWithPatch = flag.String("browser_extra_args_withpatch", "", "The extra arguments that are passed to the browser while running the benchmark for the withpatch case.")
	runID                     = flag.String("run_id", "", "The unique run id (typically requester + timestamp).")

	taskCompletedSuccessfully = false

	skiaPatchLink       = ""
	chromiumPatchLink   = ""
	customWebpagesLink  = ""
	nopatchImagesLink   = "N/A"
	withpatchImagesLink = "N/A"
)

func sendEmail(recipients []string) {
	// Send completion email.
	emailSubject := fmt.Sprintf("Pixel diff cluster telemetry task has completed (%s)", *runID)
	failureHtml := ""
	viewActionMarkup := ""
	var err error

	if taskCompletedSuccessfully {
		if viewActionMarkup, err = email.GetViewActionMarkup(fmt.Sprintf(util.SWARMING_RUN_ID_ALL_TASKS_LINK_TEMPLATE, *runID), "View Results", "Direct link to the HTML results"); err != nil {
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
	bodyTemplate := `
	The pixel diff task on %s pageset has completed. %s.<br/>
	Run description: %s<br/>
	%s
	<br/>
	The screenshots output are stored in these google storage locations:<br/>
	* %s<br/>
	* %s<br/>
	<br/>
	The patch(es) you specified are here:
	<a href='%s'>chromium</a>/<a href='%s'>skia</a>
	<br/>
	Custom webpages (if specified) are <a href='%s'>here</a>.
	<br/><br/>
	You can schedule more runs <a href='%s'>here</a>.
	<br/><br/>
	Thanks!
	`
	emailBody := fmt.Sprintf(bodyTemplate, *pagesetType, util.GetSwarmingLogsLink(*runID), *description, failureHtml, nopatchImagesLink, withpatchImagesLink, chromiumPatchLink, skiaPatchLink, customWebpagesLink, frontend.PixelDiffTasksWebapp)
	if err := util.SendEmailWithMarkup(recipients, emailSubject, emailBody, viewActionMarkup); err != nil {
		sklog.Errorf("Error while sending email: %s", err)
		return
	}
}

// TODO(rmistry): Change this once ctfe/pixel_diff is created.
func updateWebappTask() {
	vars := capture_skps.UpdateVars{}
	vars.Id = *gaeTaskID
	vars.SetCompleted(taskCompletedSuccessfully)
	skutil.LogErr(frontend.UpdateWebappTaskV2(&vars))
}

func main() {
	defer common.LogPanic()
	master_common.Init("pixel_diff")

	// Send start email.
	emailsArr := util.ParseEmails(*emails)
	emailsArr = append(emailsArr, util.CtAdmins...)
	if len(emailsArr) == 0 {
		sklog.Error("At least one email address must be specified")
		return
	}
	skutil.LogErr(frontend.UpdateWebappTaskSetStarted(&capture_skps.UpdateVars{}, *gaeTaskID))
	skutil.LogErr(util.SendTaskStartEmail(emailsArr, "Pixel diff", *runID, *description))

	// Ensure webapp is updated and completion email is sent even if task
	// fails.
	defer updateWebappTask()
	defer sendEmail(emailsArr)

	// Finish with glog flush and how long the task took.
	defer util.TimeTrack(time.Now(), "Running pixel diff task on workers")
	defer sklog.Flush()

	if *pagesetType == "" {
		sklog.Error("Must specify --pageset_type")
		return
	}
	if *description == "" {
		sklog.Error("Must specify --description")
		return
	}
	if *runID == "" {
		sklog.Error("Must specify --run_id")
		return
	}

	// Instantiate GcsUtil object.
	gs, err := util.NewGcsUtil(nil)
	if err != nil {
		sklog.Errorf("GcsUtil instantiation failed: %s", err)
		return
	}
	remoteOutputDir := filepath.Join(util.BenchmarkRunsDir, *runID)

	// Copy the patches and custom webpages to Google Storage.
	skiaPatchName := *runID + ".skia.patch"
	chromiumPatchName := *runID + ".chromium.patch"
	customWebpagesName := *runID + ".custom_webpages.csv"
	for _, patchName := range []string{skiaPatchName, chromiumPatchName, customWebpagesName} {
		if err := gs.UploadFile(patchName, os.TempDir(), remoteOutputDir); err != nil {
			sklog.Errorf("Could not upload %s to %s: %s", patchName, remoteOutputDir, err)
			return
		}
	}
	skiaPatchLink = util.GCS_HTTP_LINK + filepath.Join(util.GCSBucketName, remoteOutputDir, skiaPatchName)
	chromiumPatchLink = util.GCS_HTTP_LINK + filepath.Join(util.GCSBucketName, remoteOutputDir, chromiumPatchName)
	customWebpagesLink = util.GCS_HTTP_LINK + filepath.Join(util.GCSBucketName, remoteOutputDir, customWebpagesName)

	// Check if the patches have any content to decide if we need one or two chromium builds.
	var chromiumBuildNoPatch, chromiumBuildWithPatch string
	localPatches := []string{filepath.Join(os.TempDir(), chromiumPatchName), filepath.Join(os.TempDir(), skiaPatchName)}
	remotePatches := []string{filepath.Join(remoteOutputDir, chromiumPatchName), filepath.Join(remoteOutputDir, skiaPatchName)}
	if util.PatchesAreEmpty(localPatches) {
		// Create only one chromium build.
		chromiumBuilds, err := util.TriggerBuildRepoSwarmingTask(
			"build_chromium", *runID, "chromium", util.PLATFORM_LINUX, []string{}, remotePatches,
			/*singlebuild*/ true, 3*time.Hour, 1*time.Hour)
		if err != nil {
			sklog.Errorf("Error encountered when swarming build repo task: %s", err)
			return
		}
		if len(chromiumBuilds) != 1 {
			sklog.Errorf("Expected 1 build but instead got %d: %v.", len(chromiumBuilds), chromiumBuilds)
			return
		}
		chromiumBuildNoPatch = chromiumBuilds[0]
		chromiumBuildWithPatch = chromiumBuilds[0]

	} else {
		// Create the two required chromium builds (with patch and without the patch).
		chromiumBuilds, err := util.TriggerBuildRepoSwarmingTask(
			"build_chromium", *runID, "chromium", util.PLATFORM_LINUX, []string{}, remotePatches,
			/*singlebuild*/ false, 3*time.Hour, 1*time.Hour)
		if err != nil {
			sklog.Errorf("Error encountered when swarming build repo task: %s", err)
			return
		}
		if len(chromiumBuilds) != 2 {
			sklog.Errorf("Expected 2 builds but instead got %d: %v.", len(chromiumBuilds), chromiumBuilds)
			return
		}
		chromiumBuildNoPatch = chromiumBuilds[0]
		chromiumBuildWithPatch = chromiumBuilds[1]
	}

	// Clean up the chromium builds from Google storage after the run completes.
	defer gs.DeleteRemoteDirLogErr(filepath.Join(util.CHROMIUM_BUILDS_DIR_NAME, chromiumBuildNoPatch))
	defer gs.DeleteRemoteDirLogErr(filepath.Join(util.CHROMIUM_BUILDS_DIR_NAME, chromiumBuildWithPatch))

	// Archive, trigger and collect swarming tasks.
	isolateExtraArgs := map[string]string{
		"CHROMIUM_BUILD_NOPATCH":       chromiumBuildNoPatch,
		"CHROMIUM_BUILD_WITHPATCH":     chromiumBuildWithPatch,
		"RUN_ID":                       *runID,
		"BENCHMARK_ARGS":               *benchmarkExtraArgs,
		"BROWSER_EXTRA_ARGS_NOPATCH":   *browserExtraArgsNoPatch,
		"BROWSER_EXTRA_ARGS_WITHPATCH": *browserExtraArgsWithPatch,
	}
	customWebPagesFilePath := filepath.Join(os.TempDir(), customWebpagesName)
	numPages, err := util.GetNumPages(*pagesetType, customWebPagesFilePath)
	if err != nil {
		sklog.Errorf("Error encountered when calculating number of pages: %s", err)
		return
	}
	if _, err := util.TriggerSwarmingTask(*pagesetType, "pixel_diff", util.PIXEL_DIFF_ISOLATE, *runID, 3*time.Hour, 1*time.Hour, util.USER_TASKS_PRIORITY, MAX_PAGES_PER_SWARMING_BOT, numPages, isolateExtraArgs, util.GCE_WORKER_DIMENSIONS, 1); err != nil {
		sklog.Errorf("Error encountered when swarming tasks: %s", err)
		return
	}
	nopatchImagesLink = "gs://" + filepath.Join(util.GCSBucketName, util.BenchmarkRunsDir, fmt.Sprintf("%s-nopatch", *runID))
	withpatchImagesLink = "gs://" + filepath.Join(util.GCSBucketName, util.BenchmarkRunsDir, fmt.Sprintf("%s-withpatch", *runID))

	taskCompletedSuccessfully = true
}