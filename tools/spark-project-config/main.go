package main

import (
	"bytes"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/nexu-io/looper/internal/config"
)

//go:embed openmotion.toml
var openMotionProject []byte

func main() {
	input := flag.String("config", "", "Existing Looper TOML configuration")
	repo := flag.String("repo-path", "", "Absolute path to the OpenMotion checkout")
	output := flag.String("output", "", "New TOML candidate path")
	flag.Parse()

	if err := prepareCandidate(*input, *repo, *output); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("OpenMotion configuration candidate validated.")
}

func prepareCandidate(input, repo, output string) error {
	if filepath.Ext(input) != ".toml" || filepath.Ext(output) != ".toml" || !filepath.IsAbs(repo) {
		return errors.New("supply an existing TOML configuration, a new TOML output path, and an absolute repository path")
	}
	repo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		return errors.New("cannot resolve the OpenMotion checkout")
	}
	root, err := gitValue(repo, "rev-parse", "--show-toplevel")
	if err != nil || root != repo {
		return errors.New("repo-path must name the root of an OpenMotion checkout")
	}
	origin, err := gitValue(repo, "config", "--local", "--get", "remote.origin.url")
	if err != nil {
		return errors.New("OpenMotion checkout must have an origin")
	}
	switch strings.TrimSuffix(origin, ".git") {
	case "https://github.com/nexu-io/html-anything", "git@github.com:nexu-io/html-anything", "ssh://git@github.com/nexu-io/html-anything":
	default:
		return errors.New("checkout origin must be nexu-io/html-anything on GitHub")
	}

	if _, err := gitValue(repo, "show-ref", "--hash", "--verify", "refs/remotes/origin/feature/motion"); err != nil {
		return errors.New("checkout must have fetched origin/feature/motion")
	}

	base, err := os.ReadFile(input)
	if err != nil {
		return fmt.Errorf("read existing configuration: %w", err)
	}
	project := bytes.Replace(openMotionProject, []byte(`"__OPENMOTION_REPO_PATH__"`), []byte(strconv.Quote(filepath.Clean(repo))), 1)
	candidate := base
	if !bytes.HasSuffix(base, project) {
		candidate = append(append(base, '\n', '\n'), project...)
	}

	file, err := os.CreateTemp(filepath.Dir(output), ".looper-candidate-*.toml")
	if err != nil {
		return fmt.Errorf("stage candidate: %w", err)
	}
	defer os.Remove(file.Name())
	if err := writeAndValidate(file, candidate, repo); err != nil {
		return err
	}
	if err := os.Link(file.Name(), output); err != nil {
		return fmt.Errorf("publish new candidate: %w", err)
	}
	return nil
}

func writeAndValidate(file *os.File, candidate []byte, repo string) error {
	_, writeErr := file.Write(candidate)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return fmt.Errorf("write candidate: %w", err)
	}
	loaded, err := config.LoadFile(config.LoadFileOptions{ConfigPathOverride: file.Name()})
	if err != nil || !loaded.Metadata.ConfigFilePresent {
		var validation *config.ConfigValidationError
		if errors.As(err, &validation) {
			var fields []string
			for _, issue := range validation.Issues {
				fields = append(fields, issue.Path)
			}
			return fmt.Errorf("Looper rejected candidate fields: %s", strings.Join(fields, ", "))
		}
		// A parse failure may include parts of the original configuration.
		return errors.New("Looper rejected the candidate configuration")
	}
	for _, project := range loaded.Config.Projects {
		if project.ID != "openmotion" {
			continue
		}
		if project.Repo != "nexu-io/html-anything" || project.RepoPath != repo || project.BaseBranch == nil || *project.BaseBranch != "feature/motion" {
			break
		}
		roles := config.ProjectRoleConfigs(loaded.Config, project.ID)
		for _, skill := range loaded.Config.Roles.Reviewer.Skills.Required {
			if !slices.Contains(roles.Reviewer.Skills.Required, skill) {
				return errors.New("candidate would replace an inherited required Reviewer skill")
			}
		}
		if roles.Reviewer.Skills.Mode != config.ReviewerSkillsModeExtend || !slices.Contains(roles.Reviewer.Skills.Required, "maintainability-review") {
			break
		}
		if roles.Planner.AutoDiscovery || roles.Worker.AutoDiscovery || roles.Fixer.AutoDiscovery ||
			roles.Coordinator.Enabled || roles.Reviewer.Discovery.AutoDiscovery || roles.Reviewer.AutoMerge.Enabled {
			break
		}
		return nil
	}
	return errors.New("candidate does not resolve to the manual OpenMotion Reviewer pilot")
}

func gitValue(repo string, args ...string) (string, error) {
	output, err := exec.Command("git", append([]string{"-C", repo}, args...)...).Output()
	return strings.TrimSpace(string(output)), err
}
