package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Getting tt (§8.2, review CRITICAL-3). The driver never reads a tt working
// tree and never writes into the tt repository: it reads git OBJECTS through
// `git archive` (GIT_OPTIONAL_LOCKS=0, so no index refresh), or a tarball an
// operator made the same way, or a tree already extracted from one. Whatever
// the form, the source is copied INTO the sandbox and built there.

type ttSource struct {
	repo, rev, tar, src string
}

// ttOrigin is what the report says about where tt came from.
type ttOrigin struct {
	Rev    string // the revision asked for
	How    string // how the source was obtained, in words
	Commit string // the commit git resolved Rev to; "" when the driver could not resolve it
}

const ttDefaultRev = "51ec374"

// fetch puts tt's source under the sandbox's src/ directory.
func (t ttSource) fetch(sb *sandbox) (ttOrigin, error) {
	switch {
	case t.repo != "":
		commit, err := gitOutput(t.repo, "rev-parse", t.rev+"^{commit}")
		if err != nil {
			return ttOrigin{}, err
		}
		cmd := exec.Command("git", "-C", t.repo, "archive", "--format=tar", t.rev+":tools/tt")
		cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.StdoutPipe()
		if err != nil {
			return ttOrigin{}, err
		}
		if err := cmd.Start(); err != nil {
			return ttOrigin{}, fmt.Errorf("git archive: %w", err)
		}
		xerr := sb.extractTar("src", out)
		werr := cmd.Wait()
		if xerr != nil {
			return ttOrigin{}, xerr
		}
		if werr != nil {
			return ttOrigin{}, fmt.Errorf("git archive %s:tools/tt: %w: %s", t.rev, werr, strings.TrimSpace(stderr.String()))
		}
		return ttOrigin{Rev: t.rev, Commit: strings.TrimSpace(commit),
			How: fmt.Sprintf("git -C %s archive %s:tools/tt", t.repo, t.rev)}, nil
	case t.tar != "":
		f, err := os.Open(t.tar)
		if err != nil {
			return ttOrigin{}, err
		}
		defer f.Close()
		if err := sb.extractTar("src", f); err != nil {
			return ttOrigin{}, err
		}
		return ttOrigin{Rev: t.rev, How: "the tarball " + t.tar + " (rev " + t.rev + " as declared, not verified)"}, nil
	default:
		if err := sb.copyFS("src", os.DirFS(t.src), nil); err != nil {
			return ttOrigin{}, err
		}
		return ttOrigin{Rev: t.rev, How: "the extracted tree " + t.src + " (rev " + t.rev + " as declared, not verified)"}, nil
	}
}

func gitOutput(repo string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// build compiles tt into the sandbox's bin/tt. Missing modules download into
// the user's module cache, which is outside both protected trees.
func build(sb *sandbox) error {
	cmd := exec.Command("go", "build", "-C", sb.path("src"), "-trimpath", "-o", sb.path("bin/tt"), ".")
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "GOWORK=") && !strings.HasPrefix(e, "GOFLAGS=") {
			env = append(env, e)
		}
	}
	cmd.Env = append(env, "GOWORK=off", "GOFLAGS=-mod=readonly")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := runBounded(cmd, 10*time.Minute); err != nil {
		return fmt.Errorf("building tt: %w\n%s", err, out.String())
	}
	return nil
}

// runBounded starts cmd and waits at most bound for it. A child still running
// after the bound is reported and NOT signalled: the driver sends no signal
// (§8.2 step 9). That is also why this is not exec.CommandContext, whose
// cancellation kills.
func runBounded(cmd *exec.Cmd, bound time.Duration) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("%s (pid %d) did not exit within %s; it was left running, unsignalled", cmd.Path, cmd.Process.Pid, bound)
	}
}

// runTT runs `tt sessions --json --all` with exactly env, and returns stdout.
// tt's own exit code 0 is the only success.
func runTT(sb *sandbox, env []string) ([]byte, string, error) {
	cmd := exec.Command(sb.path("bin/tt"), "sessions", "--json", "--all")
	cmd.Env = env
	cmd.Dir = sb.path(".")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := runBounded(cmd, time.Minute); err != nil {
		return nil, stderr.String(), fmt.Errorf("tt sessions: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), stderr.String(), nil
}

// stubs writes bin/claude and bin/tmux into a scenario directory (§8.2 step
// 5). claude prints the listing and exits at once, so tt's spawn timeout (and
// the SIGKILL behind it) cannot fire; with no listing it exits 127, as a
// missing command does. tmux exits 1, so tt reaches no real tmux.
func stubs(sb *sandbox, dir, listing string) error {
	claude := "#!/bin/sh\nexit 127\n"
	if listing != "" {
		claude = "#!/bin/sh\nexec /bin/cat '" + listing + "'\n"
	}
	if err := sb.writeFile(dir+"/bin/claude", []byte(claude), 0o755); err != nil {
		return err
	}
	return sb.writeFile(dir+"/bin/tmux", []byte("#!/bin/sh\nexit 1\n"), 0o755)
}
