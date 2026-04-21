package version

const (
	defaultVersion       = "0.2.1"
	defaultVersionSource = "internal/version/version.go"
)

// These variables are shared by all Go binaries and can be overridden at build
// time with -ldflags.
var (
	Value          = defaultVersion
	VersionSource  = defaultVersionSource
	GitCommitSHA   = ""
	BuildTimestamp = ""
)

type BuildMetadata struct {
	VersionSource  string  `json:"versionSource"`
	GitCommitSHA   *string `json:"gitCommitSha"`
	BuildTimestamp *string `json:"buildTimestamp"`
}

type Info struct {
	Version  string        `json:"version"`
	Metadata BuildMetadata `json:"metadata"`
}

func Current() Info {
	return Info{
		Version: Value,
		Metadata: BuildMetadata{
			VersionSource:  VersionSource,
			GitCommitSHA:   stringPtrOrNil(GitCommitSHA),
			BuildTimestamp: stringPtrOrNil(BuildTimestamp),
		},
	}
}

func stringPtrOrNil(value string) *string {
	if value == "" {
		return nil
	}

	return &value
}
