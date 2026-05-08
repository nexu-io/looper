package disclosure

import (
	"fmt"
	"regexp"
	"runtime"
	"strings"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/version"
)

const (
	Marker        = "<!-- looper:stamp v=1 -->"
	RepoURL       = "https://github.com/nexu-io/looper"
	LegacyRepoURL = "https://github.com/powerformer/looper"
	RepoLinkHTML  = `<a href="` + RepoURL + `">Looper</a>`
	Slogan        = "An autonomous AI dev team for your GitHub repos."
	Emoji         = "🔁"

	ChannelGitCommit     = "gitCommit"
	ChannelPullRequest   = "pullRequest"
	ChannelIssueComment  = "issueComment"
	ChannelReviewComment = "reviewComment"
)

var (
	markdownStampPattern = regexp.MustCompile(`(?s)\n*(?:<!-- looper:stamp v=1 -->\n)?<sub>(?:🔁 )?(?:Generated|Powered) by (?:looper|Looper|\[Looper\]\(https://github\.com/(?:nexu-io|powerformer)/looper\)|<a href="https://github\.com/nexu-io/looper">Looper</a>).*?</sub>\s*`)
	markerOnlyPattern    = regexp.MustCompile(`(?m)\n*<!-- looper:stamp v=1 -->\s*`)
	commitTrailerPattern = regexp.MustCompile(`(?m)^Generated-By: looper .*$`)
)

type Stamper struct {
	Config  config.DisclosureConfig
	Version string
	Agent   string
	Model   string
}

func FromConfig(cfg config.Config) Stamper {
	agent := ""
	if cfg.Agent.Vendor != nil {
		agent = string(*cfg.Agent.Vendor)
	}
	model := ""
	if cfg.Agent.Model != nil {
		model = *cfg.Agent.Model
	}
	return Stamper{Config: cfg.Disclosure, Version: version.Current().Version, Agent: agent, Model: model}
}

func (s Stamper) CommitMessage(message, runner string) string {
	if !s.enabled(ChannelGitCommit) {
		return message
	}
	trailer := s.commitTrailer(runner)
	if commitTrailerPattern.MatchString(message) {
		return commitTrailerPattern.ReplaceAllString(message, trailer)
	}
	trimmed := strings.TrimRight(message, "\n")
	if trimmed == "" {
		return trailer
	}
	return trimmed + "\n\n" + trailer
}

func (s Stamper) Markdown(body, runner, channel string) string {
	if !s.enabled(channel) {
		return body
	}
	stamp := s.MarkdownStamp(runner)
	cleaned := strings.TrimRight(markdownStampPattern.ReplaceAllString(body, ""), "\n")
	if cleaned == "" {
		return stamp
	}
	return cleaned + "\n\n" + stamp
}

func (s Stamper) MarkdownStamp(runner string) string {
	return Marker + "\n" + s.markdownFooter(runner)
}

func StripMarkdownStamp(body string) string {
	cleaned := markdownStampPattern.ReplaceAllString(body, "")
	cleaned = markerOnlyPattern.ReplaceAllString(cleaned, "")
	return strings.TrimSpace(cleaned)
}

func (s Stamper) ReviewComment(body, runner string) string {
	if !s.enabled(ChannelReviewComment) {
		return body
	}
	if s.Config.Channels.InlineCommentVisible {
		return s.Markdown(body, runner, ChannelReviewComment)
	}
	cleaned := strings.TrimRight(markerOnlyPattern.ReplaceAllString(body, ""), "\n")
	if cleaned == "" {
		return Marker
	}
	return cleaned + "\n\n" + Marker
}

func (s Stamper) enabled(channel string) bool {
	if !s.Config.Enabled {
		return false
	}
	switch channel {
	case ChannelGitCommit:
		return s.Config.Channels.GitCommit
	case ChannelPullRequest:
		return s.Config.Channels.PullRequest
	case ChannelIssueComment:
		return s.Config.Channels.IssueComment
	case ChannelReviewComment:
		return s.Config.Channels.ReviewComment
	default:
		return false
	}
}

func (s Stamper) markdownFooter(runner string) string {
	parts := []string{Emoji + " Powered by " + RepoLinkHTML}
	for _, attr := range s.attributes(runner) {
		parts = append(parts, attr)
	}
	parts = append(parts, Slogan)
	return "<sub>" + strings.Join(parts, " · ") + "</sub>"
}

func (s Stamper) commitTrailer(runner string) string {
	attrs := s.attributes(runner)
	if len(attrs) == 0 {
		return "Generated-By: looper " + safeValue(s.Version)
	}
	return "Generated-By: looper " + safeValue(s.Version) + " (" + strings.Join(attrs, ", ") + ")"
}

func (s Stamper) attributes(runner string) []string {
	attrs := []string{}
	if runner = strings.TrimSpace(runner); runner != "" {
		attrs = append(attrs, "runner="+runner)
	}
	if s.Config.IncludeAgent {
		if agent := strings.TrimSpace(s.Agent); agent != "" {
			attrs = append(attrs, "agent="+agent)
		}
		if model := safeAttributeValue(s.Model); model != "" {
			attrs = append(attrs, "model="+model)
		}
	}
	if s.Config.IncludeOS {
		if osFamily := osFamily(runtime.GOOS); osFamily != "" {
			attrs = append(attrs, "os="+osFamily)
		}
	}
	return attrs
}

func osFamily(goos string) string {
	switch goos {
	case "darwin":
		return "macOS"
	case "linux":
		return "Linux"
	case "windows":
		return "Windows"
	default:
		return ""
	}
}

func safeValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "0.0.0-dev"
	}
	return value
}

func safeAttributeValue(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return ""
	}

	var b strings.Builder
	for i := 0; i < len(value); i++ {
		c := value[i]
		if isSafeAttributeByte(c) {
			b.WriteByte(c)
			continue
		}
		_, _ = fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

func isSafeAttributeByte(c byte) bool {
	return (c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') ||
		c == '-' || c == '_' || c == '.' || c == '/' || c == ':' || c == '+' || c == '@'
}
