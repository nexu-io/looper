package forge

import (
	"strconv"
	"strings"
)

// RereviewRequestID extracts the Forgejo request ID from a run's publication ID.
func RereviewRequestID(publicationID string) int64 {
	_, suffix, ok := strings.Cut(publicationID, ":rereview:")
	if !ok || !strings.HasPrefix(publicationID, "reviewer:") {
		return 0
	}
	id, err := strconv.ParseInt(suffix, 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}
