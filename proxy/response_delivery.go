package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"kiro-go/logger"
	"net"
	"net/http"
	"time"
)

// These observations end at the HTTP writer, not at a remote client's receipt.
// Only fixed categories and byte counts are retained, never payloads/errors.
type responseDelivery struct {
	Status         string `json:"status"`
	Stage          string `json:"stage,omitempty"`
	Cause          string `json:"cause,omitempty"`
	BytesAttempted int    `json:"bytesAttempted"`
	BytesWritten   int    `json:"bytesWritten"`
	DurationMs     int64  `json:"durationMs"`
}

func writeJSONWithDelivery(ctx context.Context, w http.ResponseWriter, value interface{}, entry *requestLogEntry, startedAt time.Time) {
	if ctx == nil {
		ctx = context.Background()
	}
	start := time.Now()
	delivery := &responseDelivery{}
	defer func() {
		delivery.DurationMs = time.Since(start).Milliseconds()
		entry.Delivery = delivery
		entry.DurationMs = requestDurationMs(startedAt)
		if delivery.Status == "failed" {
			entry.Status = "delivery_failed"
			entry.Error = "Downstream response " + delivery.Stage + " failed (" + delivery.Cause + ")"
			logger.Warnf("[ResponseDelivery] request_id=%s stage=%s cause=%s bytes=%d/%d", requestIDFromContext(ctx), delivery.Stage, delivery.Cause, delivery.BytesWritten, delivery.BytesAttempted)
		}
	}()
	fail := func(stage string, err error) {
		delivery.Status, delivery.Stage, delivery.Cause = "failed", stage, responseDeliveryCause(err)
	}
	data, err := json.Marshal(value)
	if err != nil {
		fail("encode", err)
		entry.StatusCode = http.StatusInternalServerError
		http.Error(w, "Response encoding failed", http.StatusInternalServerError)
		return
	}
	data = append(data, '\n')
	delivery.BytesAttempted = len(data)
	if err := ctx.Err(); err != nil {
		fail("before_write", err)
		entry.StatusCode = 499
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	delivery.BytesWritten, err = w.Write(data)
	if err == nil && delivery.BytesWritten != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		fail("write", err)
		return
	}
	err = http.NewResponseController(w).Flush()
	if errors.Is(err, http.ErrNotSupported) {
		delivery.Status = "written"
	} else if err != nil {
		fail("flush", err)
		return
	} else {
		delivery.Status = "flushed"
	}
	if err := ctx.Err(); err != nil {
		fail("after_write", err)
	}
}

func responseDeliveryCause(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, io.ErrShortWrite):
		return "short_write"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	return "io_error"
}
