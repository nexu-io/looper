package github

import (
	"fmt"
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

func reviewQualityGateApplies(event string, body string) (bool, error) {
	event = strings.ToUpper(strings.TrimSpace(event))
	markers := parseReviewIdempotencyMarkers(body)
	markerComments := reviewMarkerRE.FindAllStringSubmatch(body, -1)
	if len(markerComments) == 0 {
		return true, nil
	}
	if len(markerComments) != 1 || len(markers) != 1 {
		return true, fmt.Errorf("review body must contain exactly one well-formed looper review marker")
	}
	marker := markers[0]
	switch event {
	case "APPROVE":
		if marker.Outcome != "clean" {
			return true, fmt.Errorf("review marker outcome=%s does not match APPROVE event", marker.Outcome)
		}
	case "REQUEST_CHANGES":
		if marker.Outcome != "actionable" {
			return true, fmt.Errorf("review marker outcome=%s does not match REQUEST_CHANGES event", marker.Outcome)
		}
	}
	return marker.Outcome != "clean", nil
}

func normalizeReviewCommentAnchor(comment ReviewComment) ReviewComment {
	comment.Side = normalizeReviewCommentSide(comment.Side)
	if comment.StartLine <= 0 || comment.StartLine == comment.Line {
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
