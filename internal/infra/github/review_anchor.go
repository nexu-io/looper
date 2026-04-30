package github

import (
	"strings"

	"github.com/powerformer/looper/internal/diffanchor"
)

type reviewQualityFlag struct {
	Kind   string
	Detail string
}

func normalizeReviewAnchors(body string, comments []ReviewComment, anchors *diffanchor.Index) (string, []ReviewComment, []reviewQualityFlag) {
	flags := []reviewQualityFlag{}
	if anchors == nil {
		if len(comments) == 0 && strings.TrimSpace(body) != "" {
			if result := diffanchor.ValidateTopLevelLocation(body); result.QualityFlagged {
				flags = append(flags, reviewQualityFlag{Kind: "top-level-location-missing", Detail: result.Reason})
			}
		}
		return body, comments, flags
	}
	kept := make([]ReviewComment, 0, len(comments))
	downgraded := []string{}
	for _, comment := range comments {
		result := anchors.Validate(diffanchor.Anchor{Path: comment.Path, Line: comment.Line, Side: comment.Side, StartLine: comment.StartLine, StartSide: comment.StartSide})
		if result.Valid {
			kept = append(kept, normalizeReviewCommentAnchor(comment))
			continue
		}
		downgraded = append(downgraded, diffanchor.DowngradeBody(comment.Body, diffanchor.Anchor{Path: comment.Path, Line: comment.Line, Side: comment.Side, StartLine: comment.StartLine, StartSide: comment.StartSide}, result.Reason))
		if result.QualityFlagged {
			flags = append(flags, reviewQualityFlag{Kind: "top-level-location-missing", Detail: result.Reason})
		}
	}
	if len(downgraded) > 0 {
		parts := []string{}
		if strings.TrimSpace(body) != "" {
			parts = append(parts, strings.TrimSpace(body))
		}
		parts = append(parts, downgraded...)
		body = strings.Join(parts, "\n\n")
	}
	if len(kept) == 0 && strings.TrimSpace(body) != "" {
		if result := diffanchor.ValidateTopLevelLocation(body); result.QualityFlagged {
			flags = append(flags, reviewQualityFlag{Kind: "top-level-location-missing", Detail: result.Reason})
		}
	}
	return body, kept, flags
}

func formatReviewQualityFlags(flags []reviewQualityFlag) string {
	parts := make([]string, 0, len(flags))
	for _, flag := range flags {
		part := strings.TrimSpace(flag.Kind)
		if detail := strings.TrimSpace(flag.Detail); detail != "" {
			part += " (" + detail + ")"
		}
		if part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, "; ")
}

func reviewQualityGateApplies(event string, body string) bool {
	if strings.EqualFold(strings.TrimSpace(event), "REQUEST_CHANGES") {
		return true
	}
	body = strings.ToLower(body)
	if strings.Contains(body, "outcome=actionable") {
		return true
	}
	if strings.Contains(body, "outcome=clean") || strings.EqualFold(strings.TrimSpace(event), "APPROVE") {
		return false
	}
	return true
}

func normalizeReviewCommentAnchor(comment ReviewComment) ReviewComment {
	comment.Side = normalizeReviewCommentSide(comment.Side)
	if comment.StartLine <= 0 {
		comment.StartLine = 0
		comment.StartSide = ""
		return comment
	}
	comment.StartSide = normalizeReviewCommentSide(comment.StartSide)
	if comment.StartSide == "" {
		comment.StartSide = comment.Side
	}
	return comment
}

func normalizeReviewCommentSide(side string) string {
	side = strings.ToUpper(strings.TrimSpace(side))
	if side == diffanchor.SideLeft || side == diffanchor.SideRight {
		return side
	}
	return ""
}
