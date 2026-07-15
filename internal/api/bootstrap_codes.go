package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

const (
	bootstrapCodeTTL     = 60 * time.Second
	bootstrapCodeBytes   = 16 // 128 bits
	bootstrapMaxStore    = 256
	bootstrapInvalidMsg  = "Invalid or expired bootstrap code"
	bootstrapBodyMaxSize = 4 << 10 // 4 KiB
)

var (
	errBootstrapInvalid = errors.New(bootstrapInvalidMsg)
	errBootstrapFull    = errors.New("bootstrap code store is full")
)

type bootstrapCodes struct {
	mu       sync.Mutex
	codes    map[string]time.Time // code -> expiresAt
	now      func() time.Time
	readRand func([]byte) (int, error)
	ttl      time.Duration
	maxSize  int
}

func newBootstrapCodes() *bootstrapCodes {
	return &bootstrapCodes{
		codes:    make(map[string]time.Time),
		now:      time.Now,
		readRand: rand.Read,
		ttl:      bootstrapCodeTTL,
		maxSize:  bootstrapMaxStore,
	}
}

// Mint creates a one-shot bootstrap code that expires after TTL.
func (b *bootstrapCodes) Mint(now time.Time) (code string, expiresAt time.Time, err error) {
	if b == nil {
		return "", time.Time{}, errBootstrapInvalid
	}
	if now.IsZero() {
		now = b.now()
	}

	raw := make([]byte, bootstrapCodeBytes)
	if _, err := b.readRand(raw); err != nil {
		return "", time.Time{}, err
	}
	code = hex.EncodeToString(raw)
	expiresAt = now.Add(b.ttl)

	b.mu.Lock()
	defer b.mu.Unlock()
	b.purgeExpiredLocked(now)
	if len(b.codes) >= b.maxSize {
		return "", time.Time{}, errBootstrapFull
	}
	b.codes[code] = expiresAt
	return code, expiresAt, nil
}

// Exchange atomically consumes a valid, unexpired code. Reuse and expiry both
// return the same generic error.
func (b *bootstrapCodes) Exchange(code string, now time.Time) error {
	if b == nil {
		return errBootstrapInvalid
	}
	if now.IsZero() {
		now = b.now()
	}
	code = trimBootstrapCode(code)
	if code == "" {
		return errBootstrapInvalid
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.purgeExpiredLocked(now)

	expiresAt, ok := b.codes[code]
	if !ok || !now.Before(expiresAt) {
		delete(b.codes, code)
		return errBootstrapInvalid
	}
	delete(b.codes, code)
	return nil
}

func (b *bootstrapCodes) purgeExpiredLocked(now time.Time) {
	for code, expiresAt := range b.codes {
		if !now.Before(expiresAt) {
			delete(b.codes, code)
		}
	}
}

func trimBootstrapCode(code string) string {
	// Keep simple: no whitespace-only codes.
	for len(code) > 0 && (code[0] == ' ' || code[0] == '\t' || code[0] == '\n' || code[0] == '\r') {
		code = code[1:]
	}
	for len(code) > 0 {
		last := code[len(code)-1]
		if last != ' ' && last != '\t' && last != '\n' && last != '\r' {
			break
		}
		code = code[:len(code)-1]
	}
	return code
}
