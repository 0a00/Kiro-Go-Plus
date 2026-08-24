package proxy

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"testing"
)

func BenchmarkParseEventStreamText(b *testing.B) {
	var stream bytes.Buffer
	for index := 0; index < 64; index++ {
		stream.Write(benchmarkEventStreamFrame("assistantResponseEvent", map[string]interface{}{"content": "streaming response fragment"}))
	}
	stream.Write(benchmarkEventStreamFrame("metadataEvent", map[string]interface{}{"stopReason": "end_turn"}))
	data := stream.Bytes()
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if err := parseEventStream(bytes.NewReader(data), &KiroStreamCallback{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseEventStreamZeroArgumentTool(b *testing.B) {
	const toolName = "mcpMemoryReadGraphH123"
	data := benchmarkEventStreamFrame("toolUseEvent", map[string]interface{}{
		"toolUseId": "toolu_zero",
		"name":      toolName,
		"stop":      true,
	})
	options := eventStreamParseOptionsForPayload(payloadWithTestTool(toolName, map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{},
	}))
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if err := parseEventStreamWithOptions(bytes.NewReader(data), &KiroStreamCallback{}, options); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkEventStreamFrame(eventType string, payload map[string]interface{}) []byte {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	headerValue := []byte(eventType)
	headers := make([]byte, 0, 1+len(":event-type")+1+2+len(headerValue))
	headers = append(headers, byte(len(":event-type")))
	headers = append(headers, []byte(":event-type")...)
	headers = append(headers, byte(7))
	headers = append(headers, byte(len(headerValue)>>8), byte(len(headerValue)))
	headers = append(headers, headerValue...)

	totalLength := 12 + len(headers) + len(payloadBytes) + 4
	frame := make([]byte, 12, totalLength)
	binary.BigEndian.PutUint32(frame[0:4], uint32(totalLength))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(headers)))
	binary.BigEndian.PutUint32(frame[8:12], crc32.ChecksumIEEE(frame[0:8]))
	frame = append(frame, headers...)
	frame = append(frame, payloadBytes...)
	frame = append(frame, 0, 0, 0, 0)
	binary.BigEndian.PutUint32(frame[len(frame)-4:], crc32.ChecksumIEEE(frame[:len(frame)-4]))
	return frame
}
