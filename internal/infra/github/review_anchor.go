package github

import (
	"fmt"
	"strings"

	"github.com/nexu-io/looper/internal/diffanchor"
)

const nearestReviewAnchorMaxDistance int64 = 3

type reviewQualityFlag struct {
	Kind   string
	Detail string
}

type reviewCommentProcessing struct {
	OriginalCount   int
	SubmittedCount  int
	NormalizedCount int
	DowngradedCount int
	DroppedCount    int
	Comments        []map[string]any
}

func normalizeReviewAnchors(body string, comments []ReviewComment, anchors *diffanchor.Index) (string, []ReviewComment, []reviewQualityFlag, reviewCommentProcessing) {
	flags := []reviewQualityFlag{}
	processing := reviewCommentProcessing{OriginalCount: len(comments), Comments: make([]map[string]any, 0, len(comments))}
	if anchors == nil {
		if len(comments) == 0 && strings.TrimSpace(body) != "" {
			if result := diffanchor.ValidateTopLevelLocation(body); result.QualityFlagged {
				flags = append(flags, reviewQualityFlag{Kind: "top-level-location-missing", Detail: result.Reason})
			}
		}
		processing.SubmittedCount = len(comments)
		for idx := range comments {
			comments[idx].DiagnosticIndex = idx
			processing.Comments = append(processing.Comments, reviewCommentProcessingEntry(idx, "kept", comments[idx], comments[idx], ""))
		}
		return body, comments, flags, processing
	}
	kept := make([]ReviewComment, 0, len(comments))
	downgraded := []string{}
	for idx, comment := range comments {
		original := comment
		anchor := diffanchor.Anchor{Path: comment.Path, Line: comment.Line, Side: comment.Side, StartLine: comment.StartLine, StartSide: comment.StartSide}
		result := anchors.Validate(anchor)
		if result.Valid {
			comment.DiagnosticIndex = idx
			normalized := normalizeReviewCommentAnchor(comment)
			kept = append(kept, normalized)
			action := "kept"
			if reviewCommentAnchorChanged(original, normalized) {
				processing.NormalizedCount++
				action = "normalized"
			}
			processing.Comments = append(processing.Comments, reviewCommentProcessingEntry(idx, action, original, normalized, ""))
			continue
		}
		if nearest, ok := nearestSafeReviewAnchor(*anchors, anchor); ok {
			comment.DiagnosticIndex = idx
			comment.Line = nearest.Line
			comment.Side = nearest.Side
			comment.StartLine = nearest.StartLine
			comment.StartSide = nearest.StartSide
			comment.Body = addOriginalReviewLocation(comment.Body, anchor)
			normalized := normalizeReviewCommentAnchor(comment)
			kept = append(kept, normalized)
			processing.NormalizedCount++
			processing.Comments = append(processing.Comments, reviewCommentProcessingEntry(idx, "retargeted", original, normalized, result.Reason))
			continue
		}
		downgraded = append(downgraded, diffanchor.FallbackBody(comment.Body, anchor, result.Reason))
		processing.DowngradedCount++
		processing.Comments = append(processing.Comments, reviewCommentProcessingEntry(idx, "downgraded", original, ReviewComment{}, result.Reason))
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
	processing.SubmittedCount = len(kept)
	return body, kept, flags, processing
}

func reviewCommentProcessingEntry(index int, action string, original ReviewComment, final ReviewComment, reason string) map[string]any {
	entry := map[string]any{"index": index, "action": action}
	if originalAnchor := reviewCommentAnchorMap(original); len(originalAnchor) > 0 {
		entry["original_anchor"] = originalAnchor
	}
	if finalAnchor := reviewCommentAnchorMap(final); len(finalAnchor) > 0 {
		entry["final_anchor"] = finalAnchor
	}
	if strings.TrimSpace(reason) != "" {
		entry["reason"] = strings.TrimSpace(reason)
	}
	return entry
}

func reviewCommentAnchorChanged(before ReviewComment, after ReviewComment) bool {
	return before.Path != after.Path || before.Line != after.Line || normalizeReviewCommentSide(before.Side) != after.Side || before.StartLine != after.StartLine || normalizeReviewCommentSide(before.StartSide) != after.StartSide
}

func reviewCommentAnchorMap(comment ReviewComment) map[string]any {
	anchor := map[string]any{}
	if strings.TrimSpace(comment.Path) != "" {
		anchor["path"] = comment.Path
	}
	if comment.Line > 0 {
		anchor["line"] = comment.Line
	}
	if side := normalizeReviewCommentSide(comment.Side); side != "" {
		anchor["side"] = side
	}
	if comment.StartLine > 0 {
		anchor["start_line"] = comment.StartLine
	}
	if startSide := normalizeReviewCommentSide(comment.StartSide); startSide != "" {
		anchor["start_side"] = startSide
	}
	return anchor
}

func markReviewCommentDropped(entries []map[string]any, index int) {
	for _, entry := range entries {
		if entryIndex, ok := entry["index"].(int); ok && entryIndex == index {
			entry["action"] = "dropped"
			return
		}
	}
}

func nearestSafeReviewAnchor(idx diffanchor.Index, anchor diffanchor.Anchor) (diffanchor.Anchor, bool) {
	if anchor.Path == "" || anchor.Line <= 0 || normalizeReviewCommentSide(anchor.Side) == "" || anchor.StartLine > 0 && anchor.StartLine != anchor.Line {
		return diffanchor.Anchor{}, false
	}
	if lineFallsWithinAnyRange(idx, anchor.Path, anchor.Line) {
		return diffanchor.Anchor{}, false
	}
	side := normalizeReviewCommentSide(anchor.Side)
	var nearest diffanchor.Range
	var nearestDistance int64
	ambiguous := false
	for _, candidate := range idx.Ranges {
		if candidate.Path != anchor.Path || candidate.Side != side {
			continue
		}
		line := clampInt64(anchor.Line, candidate.Start, candidate.End)
		distance := absInt64(anchor.Line - line)
		if distance == 0 || distance > nearestReviewAnchorMaxDistance {
			continue
		}
		if nearestDistance == 0 || distance < nearestDistance {
			nearest = candidate
			nearestDistance = distance
			ambiguous = false
			continue
		}
		if distance == nearestDistance {
			ambiguous = true
		}
	}
	if nearestDistance == 0 || ambiguous {
		return diffanchor.Anchor{}, false
	}
	line := clampInt64(anchor.Line, nearest.Start, nearest.End)
	return diffanchor.Anchor{Path: anchor.Path, Line: line, Side: side}, true
}

func lineFallsWithinAnyRange(idx diffanchor.Index, path string, line int64) bool {
	for _, r := range idx.Ranges {
		if r.Path == path && r.Start <= line && r.End >= line {
			return true
		}
	}
	return false
}

func absInt64(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}

func clampInt64(value, min, max int64) int64 {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func addOriginalReviewLocation(body string, anchor diffanchor.Anchor) string {
	location := diffanchor.FallbackLocation(anchor)
	if location == "" {
		return body
	}
	location = strings.TrimPrefix(location, "Location: ")
	prefix := "Original requested location: " + location
	if trimmed := strings.TrimSpace(body); trimmed != "" {
		return prefix + "\n\n" + trimmed
	}
	return prefix
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
		if marker.Outcome != "blocking" {
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
