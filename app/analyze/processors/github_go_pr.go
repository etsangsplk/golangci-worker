package processors

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/golangci/golangci-api/pkg/app/ensuredeps"

	"github.com/golangci/golangci-worker/app/analytics"
	"github.com/golangci/golangci-worker/app/analyze/linters"
	"github.com/golangci/golangci-worker/app/analyze/linters/golinters"
	"github.com/golangci/golangci-worker/app/analyze/linters/result"
	"github.com/golangci/golangci-worker/app/analyze/prstate"
	"github.com/golangci/golangci-worker/app/analyze/repoinfo"
	"github.com/golangci/golangci-worker/app/analyze/reporters"
	"github.com/golangci/golangci-worker/app/lib/errorutils"
	"github.com/golangci/golangci-worker/app/lib/executors"
	"github.com/golangci/golangci-worker/app/lib/fetchers"
	"github.com/golangci/golangci-worker/app/lib/github"
	"github.com/golangci/golangci-worker/app/lib/goutils/workspaces"
	"github.com/golangci/golangci-worker/app/lib/httputils"
	gh "github.com/google/go-github/github"
)

const (
	patchPath = "../changes.patch"
)

type githubGoPRConfig struct {
	repoFetcher fetchers.Fetcher
	infoFetcher repoinfo.Fetcher
	linters     []linters.Linter
	runner      linters.Runner
	reporter    reporters.Reporter
	exec        executors.Executor
	client      github.Client
	state       prstate.Storage
}

type githubGoPR struct {
	pr           *gh.PullRequest
	analysisGUID string

	context *github.Context
	gw      *workspaces.Go

	githubGoPRConfig
	resultCollector
}

func newGithubGoPR(ctx context.Context, c *github.Context, cfg githubGoPRConfig, analysisGUID string) (*githubGoPR, error) {
	if cfg.client == nil {
		cfg.client = github.NewMyClient()
	}

	if cfg.exec == nil {
		var err error
		cfg.exec, err = makeExecutor(ctx, &c.Repo)
		if err != nil {
			return nil, fmt.Errorf("can't make executor: %s", err)
		}
	}

	if cfg.repoFetcher == nil {
		cfg.repoFetcher = fetchers.NewGit()
	}

	if cfg.infoFetcher == nil {
		cfg.infoFetcher = repoinfo.NewCloningFetcher(cfg.repoFetcher)
	}

	if cfg.linters == nil {
		cfg.linters = []linters.Linter{
			golinters.GolangciLint{
				PatchPath: patchPath,
			},
		}
	}

	if cfg.reporter == nil {
		cfg.reporter = reporters.NewGithubReviewer(c, cfg.client)
	}

	if cfg.runner == nil {
		cfg.runner = linters.SimpleRunner{}
	}

	if cfg.state == nil {
		cfg.state = prstate.NewAPIStorage(httputils.GrequestsClient{})
	}

	return &githubGoPR{
		context:          c,
		githubGoPRConfig: cfg,
		analysisGUID:     analysisGUID,
	}, nil
}

func storePatch(ctx context.Context, patch string, exec executors.Executor) error {
	f, err := ioutil.TempFile("/tmp", "golangci.diff")
	defer os.Remove(f.Name())

	if err != nil {
		return fmt.Errorf("can't create temp file for patch: %s", err)
	}
	if err = ioutil.WriteFile(f.Name(), []byte(patch), os.ModePerm); err != nil {
		return fmt.Errorf("can't write patch to temp file %s: %s", f.Name(), err)
	}

	if err = exec.CopyFile(ctx, patchPath, f.Name()); err != nil {
		return fmt.Errorf("can't copy patch file: %s", err)
	}

	return nil
}

func (g githubGoPR) getRepo() *fetchers.Repo {
	return &fetchers.Repo{
		CloneURL: g.context.GetCloneURL(g.pr.GetHead().GetRepo()),
		Ref:      g.pr.GetHead().GetRef(),
		FullPath: fmt.Sprintf("github.com/%s/%s", g.context.Repo.Owner, g.context.Repo.Name),
	}
}

func (g *githubGoPR) prepareRepo(ctx context.Context) error {
	repo := g.getRepo()
	var err error
	g.trackTiming("Clone", func() {
		err = g.repoFetcher.Fetch(ctx, repo, g.exec)
	})
	if err != nil {
		return &errorutils.InternalError{
			PublicDesc:  "can't clone git repo",
			PrivateDesc: fmt.Sprintf("can't clone git repo: %s", err),
		}
	}

	var depsRes *ensuredeps.Result
	g.trackTiming("Deps", func() {
		depsRes, err = g.gw.FetchDeps(ctx, repo.FullPath)
	})
	if err != nil {
		// don't public warn: it's an internal error
		analytics.Log(ctx).Warnf("Internal error fetching deps: %s", err)
	} else {
		analytics.Log(ctx).Infof("Got deps result: %#v", depsRes)

		for _, w := range depsRes.Warnings {
			warnText := fmt.Sprintf("Fetch deps: %s: %s", w.Kind, w.Text)
			warnText = escapeErrorText(warnText, g.buildSecrets())
			g.publicWarn("prepare repo", warnText)

			analytics.Log(ctx).Infof("Fetch deps warning: [%s]: %s", w.Kind, w.Text)
		}
	}

	return nil
}

func (g githubGoPR) updateAnalysisState(ctx context.Context, res *result.Result, status github.Status, publicError string) {
	resJSON := &resultJSON{
		Version: 1,
		WorkerRes: workerRes{
			Timings:  g.timings,
			Warnings: g.warnings,
			Error:    publicError,
		},
	}

	issuesCount := 0
	if res != nil {
		resJSON.GolangciLintRes = res.ResultJSON
		issuesCount = len(res.Issues)
	}
	s := &prstate.State{
		Status:              "processed/" + string(status),
		ReportedIssuesCount: issuesCount,
		ResultJSON:          resJSON,
	}

	if err := g.state.UpdateState(ctx, g.context.Repo.Owner, g.context.Repo.Name, g.analysisGUID, s); err != nil {
		analytics.Log(ctx).Warnf("Can't set analysis %s status to '%v': %s", g.analysisGUID, s, err)
	}
}

func getGithubStatusForIssues(issues []result.Issue) (github.Status, string) {
	switch len(issues) {
	case 0:
		return github.StatusSuccess, "No issues found!"
	case 1:
		return github.StatusFailure, "1 issue found"
	default:
		return github.StatusFailure, fmt.Sprintf("%d issues found", len(issues))
	}
}

func (g githubGoPR) buildSecrets() map[string]string {
	const hidden = "{hidden}"
	ret := map[string]string{
		g.context.GithubAccessToken: hidden,
		g.gw.Gopath():               "$GOPATH",
	}

	for _, kv := range os.Environ() {
		parts := strings.Split(kv, "=")
		if len(parts) != 2 {
			continue
		}

		v := parts[1]
		if len(v) >= 6 {
			ret[v] = hidden
		}
	}

	return ret
}

func (g *githubGoPR) processWithGuaranteedGithubStatus(ctx context.Context) error {
	res, err := g.work(ctx)
	analytics.Log(ctx).Infof("timings: %s", g.timings)

	ctx = context.Background() // no timeout for state and status saving: it must be durable

	var status github.Status
	var statusDesc, publicError string
	if err != nil {
		if serr, ok := err.(*IgnoredError); ok {
			status, statusDesc = serr.Status, serr.StatusDesc
			if !serr.IsRecoverable {
				err = nil
			}
			// already must have warning, don't set publicError
		} else if ierr, ok := err.(*errorutils.InternalError); ok {
			if strings.Contains(ierr.PrivateDesc, noGoFilesToAnalyzeErr) {
				status, statusDesc = github.StatusSuccess, noGoFilesToAnalyzeMessage
				err = nil
			} else {
				status, statusDesc = github.StatusError, ierr.PublicDesc
			}
			publicError = statusDesc
		} else if berr, ok := err.(*errorutils.BadInputError); ok {
			berr.PublicDesc = escapeErrorText(berr.PublicDesc, g.buildSecrets())
			status, statusDesc = github.StatusError, "can't analyze"
			publicError = berr.PublicDesc
			err = nil
			analytics.Log(ctx).Warnf("PR analysis bad input error: %s", berr)
		} else {
			status, statusDesc = github.StatusError, internalError
			publicError = statusDesc
		}
	} else {
		status, statusDesc = getGithubStatusForIssues(res.Issues)
	}

	// update of state must be before commit status update: user can open details link before: race condition
	g.updateAnalysisState(ctx, res, status, publicError)
	g.setCommitStatus(ctx, status, statusDesc)

	return err
}

func (g *githubGoPR) work(ctx context.Context) (res *result.Result, err error) {
	defer func() {
		if rerr := recover(); rerr != nil {
			err = &errorutils.InternalError{
				PublicDesc:  "golangci-worker panic-ed",
				PrivateDesc: fmt.Sprintf("panic occured: %s, %s", rerr, debug.Stack()),
			}
		}
	}()

	prState := strings.ToUpper(g.pr.GetState())
	if prState == "MERGED" || prState == "CLOSED" {
		// branch can be deleted: will be an error; no need to analyze
		g.publicWarn("process", fmt.Sprintf("Pull Request is already %s, skip analysis", prState))
		analytics.Log(ctx).Warnf("Pull Request is already %s, skip analysis", prState)
		return nil, &IgnoredError{
			Status:        github.StatusSuccess,
			StatusDesc:    fmt.Sprintf("Pull Request is already %s", strings.ToLower(prState)),
			IsRecoverable: false,
		}
	}

	if err = g.prepareRepo(ctx); err != nil {
		return nil, err // don't wrap error, need to save it's type
	}

	g.trackTiming("Analysis", func() {
		res, err = g.runner.Run(ctx, g.linters, g.exec)
	})
	if err != nil {
		return nil, err // don't wrap error, need to save it's type
	}

	issues := res.Issues
	analytics.SaveEventProp(ctx, analytics.EventPRChecked, "reportedIssues", len(issues))

	if len(issues) == 0 {
		analytics.Log(ctx).Infof("Linters found no issues")
	} else {
		analytics.Log(ctx).Infof("Linters found %d issues: %+v", len(issues), issues)
	}

	if err = g.reporter.Report(ctx, g.pr.GetHead().GetSHA(), issues); err != nil {
		return nil, &errorutils.InternalError{
			PublicDesc:  "can't send pull request comments to github",
			PrivateDesc: fmt.Sprintf("can't send pull request comments to github: %s", err),
		}
	}

	return res, nil
}

func (g githubGoPR) setCommitStatus(ctx context.Context, status github.Status, desc string) {
	var url string
	if status == github.StatusFailure || status == github.StatusSuccess || status == github.StatusError {
		c := g.context
		url = fmt.Sprintf("%s/r/github.com/%s/%s/pulls/%d",
			os.Getenv("WEB_ROOT"), c.Repo.Owner, c.Repo.Name, g.pr.GetNumber())
	}
	err := g.client.SetCommitStatus(ctx, g.context, g.pr.GetHead().GetSHA(), status, desc, url)
	if err != nil {
		g.publicWarn("github", "Can't set github commit status")
		analytics.Log(ctx).Warnf("Can't set github commit status: %s", err)
	}
}

func (g githubGoPR) Process(ctx context.Context) error {
	defer g.exec.Clean()

	var err error
	g.pr, err = g.client.GetPullRequest(ctx, g.context)
	if err != nil {
		if !github.IsRecoverableError(err) {
			return err // preserve error
		}
		return fmt.Errorf("can't get pull request: %s", err)
	}

	g.gw = workspaces.NewGo(g.exec, g.infoFetcher)
	if err = g.gw.Setup(ctx, g.getRepo(), "github.com", g.context.Repo.Owner, g.context.Repo.Name); err != nil {
		return fmt.Errorf("can't setup go workspace: %s", err)
	}
	defer g.gw.Clean(ctx)
	g.exec = g.gw.Executor()

	patch, err := g.client.GetPullRequestPatch(ctx, g.context)
	if err != nil {
		if !github.IsRecoverableError(err) {
			return err // preserve error
		}
		return fmt.Errorf("can't get patch: %s", err)
	}

	if err = storePatch(ctx, patch, g.exec); err != nil {
		return fmt.Errorf("can't store patch: %s", err)
	}

	g.setCommitStatus(ctx, github.StatusPending, "GolangCI is reviewing your Pull Request...")
	curState, err := g.state.GetState(ctx, g.context.Repo.Owner, g.context.Repo.Name, g.analysisGUID)
	if err != nil {
		analytics.Log(ctx).Warnf("Can't get current state: %s", err)
	} else if curState.Status == statusSentToQueue {
		g.addTimingFrom("In Queue", fromDBTime(curState.CreatedAt))
		inQueue := time.Since(fromDBTime(curState.CreatedAt))
		analytics.SaveEventProp(ctx, analytics.EventPRChecked, "inQueueSeconds", int(inQueue/time.Second))
		curState.Status = statusProcessing
		if err = g.state.UpdateState(ctx, g.context.Repo.Owner, g.context.Repo.Name, g.analysisGUID, curState); err != nil {
			analytics.Log(ctx).Warnf("Can't update analysis %s state with setting status to 'processing': %s", g.analysisGUID, err)
		}
	}

	return g.processWithGuaranteedGithubStatus(ctx)
}
