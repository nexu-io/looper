package version

import "strings"

const (
	packageImportPath          = "github.com/nexu-io/looper/internal/version"
	buildVersionEnvVar         = "LOOPER_BUILD_VERSION"
	buildVersionSourceEnvVar   = "LOOPER_BUILD_VERSION_SOURCE"
	buildChannelEnvVar         = "LOOPER_BUILD_CHANNEL"
	buildAPIVersionEnvVar      = "LOOPER_BUILD_API_VERSION"
	buildMinCliForDaemonEnvVar = "LOOPER_BUILD_MIN_CLI_FOR_DAEMON"
	buildMinDaemonForCliEnvVar = "LOOPER_BUILD_MIN_DAEMON_FOR_CLI"
	buildGitSHAEnvVar          = "LOOPER_BUILD_GIT_SHA"
	buildTimestampEnvVar       = "LOOPER_BUILD_TIMESTAMP"
)

type BuildOverrides struct {
	Version         string
	VersionSource   string
	Channel         string
	APIVersion      string
	MinCliForDaemon string
	MinDaemonForCli string
	GitCommitSHA    string
	BuildTimestamp  string
}

func DefaultBuildOverrides() BuildOverrides {
	return BuildOverrides{
		Version:         defaultVersion,
		VersionSource:   defaultVersionSource,
		Channel:         defaultChannel,
		APIVersion:      defaultAPIVersion,
		MinCliForDaemon: "",
		MinDaemonForCli: "",
		GitCommitSHA:    "",
		BuildTimestamp:  "",
	}
}

func BuildOverridesFromEnv(lookupEnv func(string) string) BuildOverrides {
	overrides := DefaultBuildOverrides()
	if v := strings.TrimSpace(lookupEnv(buildVersionEnvVar)); v != "" {
		overrides.Version = v
	}
	if v := strings.TrimSpace(lookupEnv(buildVersionSourceEnvVar)); v != "" {
		overrides.VersionSource = v
	}
	if v := strings.TrimSpace(lookupEnv(buildChannelEnvVar)); v != "" {
		overrides.Channel = v
	}
	if v := strings.TrimSpace(lookupEnv(buildAPIVersionEnvVar)); v != "" {
		overrides.APIVersion = v
	}
	overrides.MinCliForDaemon = strings.TrimSpace(lookupEnv(buildMinCliForDaemonEnvVar))
	overrides.MinDaemonForCli = strings.TrimSpace(lookupEnv(buildMinDaemonForCliEnvVar))
	overrides.GitCommitSHA = strings.TrimSpace(lookupEnv(buildGitSHAEnvVar))
	overrides.BuildTimestamp = strings.TrimSpace(lookupEnv(buildTimestampEnvVar))
	return overrides
}

func LDFlags(overrides BuildOverrides) string {
	return strings.Join([]string{
		ldflagAssignment("Value", overrides.Version),
		ldflagAssignment("VersionSource", overrides.VersionSource),
		ldflagAssignment("Channel", overrides.Channel),
		ldflagAssignment("APIVersion", overrides.APIVersion),
		ldflagAssignment("MinCliForDaemon", overrides.MinCliForDaemon),
		ldflagAssignment("MinDaemonForCli", overrides.MinDaemonForCli),
		ldflagAssignment("GitCommitSHA", overrides.GitCommitSHA),
		ldflagAssignment("BuildTimestamp", overrides.BuildTimestamp),
	}, " ")
}

func ldflagAssignment(name string, value string) string {
	return "-X " + packageImportPath + "." + name + "=" + value
}
