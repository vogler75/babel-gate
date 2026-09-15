package google

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// decodeSSE assembles complete SSE events. It accepts both "data:value" and
// "data: value", joins multiple data fields with a newline, ignores comments,
// and relies on bufio.ScanLines for LF and CRLF framing.
func decodeSSE(r io.Reader, maxEventBytes int, handle func([]byte) error) (frames int, sawInput bool, err error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), maxEventBytes)
	var data []string
	dataBytes := 0

	flush := func() error {
		if len(data) == 0 {
			return nil
		}
		payload := strings.Join(data, "\n")
		data = nil
		dataBytes = 0
		frames++
		return handle([]byte(payload))
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line != "" {
			sawInput = true
		}
		if line == "" {
			if err := flush(); err != nil {
				return frames, sawInput, err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			field, value = line, ""
		}
		if strings.HasPrefix(value, " ") {
			value = value[1:]
		}
		if field != "data" {
			continue
		}
		dataBytes += len(value)
		if len(data) > 0 {
			dataBytes++
		}
		if dataBytes > maxEventBytes {
			return frames, sawInput, fmt.Errorf("Google SSE event exceeds %d bytes", maxEventBytes)
		}
		data = append(data, value)
	}
	if err := scanner.Err(); err != nil {
		return frames, sawInput, err
	}
	if err := flush(); err != nil {
		return frames, sawInput, err
	}
	return frames, sawInput, nil
}
