package modelcatalog

import "errors"

// ErrUnknownVendor is returned when List is called with a non-enum vendor.
var ErrUnknownVendor = errors.New("unknown agent vendor")
