package version

import "strings"

const (
	packageImportPath    = "github.com/powerformer/looper/internal/version"
	buildGitSHAEnvVar    = "LOOPER_BUILD_GIT_SHA"
	buildTimestampEnvVar = "LOOPER_BUILD_TIMESTAMP"
)

type BuildOverrides struct {
	Version        string
	VersionSource  string
	GitCommitSHA   string
	BuildTimestamp string
}

func DefaultBuildOverrides() BuildOverrides {
	return BuildOverrides{
		Version:        defaultVersion,
		VersionSource:  defaultVersionSource,
		GitCommitSHA:   "",
		BuildTimestamp: "",
	}
}

func BuildOverridesFromEnv(lookupEnv func(string) string) BuildOverrides {
	overrides := DefaultBuildOverrides()
	overrides.GitCommitSHA = strings.TrimSpace(lookupEnv(buildGitSHAEnvVar))
	overrides.BuildTimestamp = strings.TrimSpace(lookupEnv(buildTimestampEnvVar))
	return overrides
}

func LDFlags(overrides BuildOverrides) string {
	return strings.Join([]string{
		ldflagAssignment("Value", overrides.Version),
		ldflagAssignment("VersionSource", overrides.VersionSource),
		ldflagAssignment("GitCommitSHA", overrides.GitCommitSHA),
		ldflagAssignment("BuildTimestamp", overrides.BuildTimestamp),
	}, " ")
}

func ldflagAssignment(name string, value string) string {
	return "-X " + packageImportPath + "." + name + "=" + value
}
