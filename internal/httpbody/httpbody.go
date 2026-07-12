package httpbody

import (
	"errors"
	"io"
)

const DefaultLimit int64 = 2 << 20

var ErrTooLarge = errors.New("HTTP response body exceeds configured limit")

func ReadAll(reader io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = DefaultLimit
	}
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return body[:limit], ErrTooLarge
	}
	return body, nil
}

func ReadAllTruncated(reader io.Reader, limit int64) []byte {
	body, _ := ReadAll(reader, limit)
	return body
}

func LimitReader(reader io.Reader, limit int64) io.Reader {
	if limit <= 0 {
		limit = DefaultLimit
	}
	return io.LimitReader(reader, limit)
}
