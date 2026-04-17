package version

const (
	defaultVersion       = "dev"
	defaultVersionSource = "manual"
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
	VersionSource  string
	GitCommitSHA   string
	BuildTimestamp string
}

type Info struct {
	Version  string
	Metadata BuildMetadata
}

func Current() Info {
	return Info{
		Version: Value,
		Metadata: BuildMetadata{
			VersionSource:  VersionSource,
			GitCommitSHA:   GitCommitSHA,
			BuildTimestamp: BuildTimestamp,
		},
	}
}
