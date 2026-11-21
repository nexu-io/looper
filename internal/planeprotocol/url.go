package planeprotocol

import (
	"bytes"
	"errors"
	"sort"
	"strings"
)

var upperHex = []byte("0123456789ABCDEF")

func CanonicalizePath(value string) (string, error) {
	if !strings.HasPrefix(value, "/") {
		return "", errors.New("path must be absolute")
	}
	if strings.Contains(value, "\\") || hasControl(value) {
		return "", errors.New("path contains forbidden characters")
	}
	rawSegments := strings.Split(value, "/")[1:]
	segments := make([][]byte, 0, len(rawSegments))
	for _, raw := range rawSegments {
		decoded, err := strictPercentDecode(raw)
		if err != nil {
			return "", err
		}
		if bytes.ContainsAny(decoded, "/\\") {
			return "", errors.New("encoded path separators are forbidden")
		}
		switch string(decoded) {
		case ".":
			continue
		case "..":
			if len(segments) == 0 {
				return "", errors.New("path escapes its root")
			}
			segments = segments[:len(segments)-1]
		default:
			segments = append(segments, decoded)
		}
	}
	encoded := make([]string, 0, len(segments))
	for _, segment := range segments {
		encoded = append(encoded, percentEncode(segment))
	}
	return "/" + strings.Join(encoded, "/"), nil
}

func CanonicalizeQuery(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	type pair struct{ key, value []byte }
	fields := strings.Split(value, "&")
	pairs := make([]pair, 0, len(fields))
	for _, field := range fields {
		separator := strings.IndexByte(field, '=')
		if separator < 0 {
			return "", errors.New("query fields must contain '='")
		}
		key, err := strictPercentDecode(field[:separator])
		if err != nil {
			return "", err
		}
		itemValue, err := strictPercentDecode(field[separator+1:])
		if err != nil {
			return "", err
		}
		pairs = append(pairs, pair{key: key, value: itemValue})
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		if comparison := bytes.Compare(pairs[i].key, pairs[j].key); comparison != 0 {
			return comparison < 0
		}
		return bytes.Compare(pairs[i].value, pairs[j].value) < 0
	})
	encoded := make([]string, 0, len(pairs))
	for _, item := range pairs {
		encoded = append(encoded, percentEncode(item.key)+"="+percentEncode(item.value))
	}
	return strings.Join(encoded, "&"), nil
}

func strictPercentDecode(value string) ([]byte, error) {
	raw := []byte(value)
	decoded := make([]byte, 0, len(raw))
	for index := 0; index < len(raw); {
		if raw[index] != '%' {
			decoded = append(decoded, raw[index])
			index++
			continue
		}
		if index+2 >= len(raw) {
			return nil, errors.New("truncated percent escape")
		}
		high, highOK := fromHex(raw[index+1])
		low, lowOK := fromHex(raw[index+2])
		if !highOK || !lowOK {
			return nil, errors.New("invalid percent escape")
		}
		decoded = append(decoded, high<<4|low)
		index += 3
	}
	return decoded, nil
}

func percentEncode(value []byte) string {
	encoded := make([]byte, 0, len(value))
	for _, char := range value {
		if isUnreserved(char) {
			encoded = append(encoded, char)
			continue
		}
		encoded = append(encoded, '%', upperHex[char>>4], upperHex[char&0x0f])
	}
	return string(encoded)
}

func fromHex(value byte) (byte, bool) {
	switch {
	case value >= '0' && value <= '9':
		return value - '0', true
	case value >= 'a' && value <= 'f':
		return value - 'a' + 10, true
	case value >= 'A' && value <= 'F':
		return value - 'A' + 10, true
	default:
		return 0, false
	}
}

func isUnreserved(value byte) bool {
	return value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z' || value >= '0' && value <= '9' || strings.ContainsRune("-._~", rune(value))
}

func hasControl(value string) bool {
	for _, char := range []byte(value) {
		if char < 0x20 || char == 0x7f {
			return true
		}
	}
	return false
}
