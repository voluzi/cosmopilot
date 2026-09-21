package chainnode

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	appTerminationLogMaxBytes = 64 * 1024
	sdkHaltMessage            = "halting node per configuration"
	appTerminationLogTimeout  = 3 * time.Second
)

var plainSDKHaltLine = regexp.MustCompile(`^(?:(?:\S+\s+)?(?:INF|INFO)\s+|I\[[^]]+\]\s+)halting node per configuration(?:\s|$)`)

func hasAuthoritativeHaltLog(logs []byte, haltHeight int64, startedAt, finishedAt time.Time) bool {
	if len(logs) == 0 || len(logs) > appTerminationLogMaxBytes || logs[len(logs)-1] != '\n' {
		return false
	}
	for _, line := range bytes.Split(logs, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		separator := bytes.IndexByte(line, ' ')
		if separator <= 0 {
			continue
		}
		recordedAt, err := time.Parse(time.RFC3339Nano, string(line[:separator]))
		payload := bytes.TrimSpace(line[separator+1:])
		if !bytes.Contains(payload, []byte(sdkHaltMessage)) {
			continue
		}
		if err != nil || recordedAt.Before(startedAt) || !recordedAt.Before(finishedAt.Add(time.Second)) {
			continue
		}
		var valid bool
		if payload[0] == '{' {
			valid = validJSONSDKHaltLine(payload, haltHeight)
		} else {
			valid = validPlainSDKHaltLine(string(payload), haltHeight)
		}
		if !valid {
			continue
		}
		return true
	}
	return false
}

func validJSONSDKHaltLine(line []byte, haltHeight int64) bool {
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return false
	}
	var message, level string
	var height, haltTime *int64
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return false
		}
		var value any
		if err := decoder.Decode(&value); err != nil {
			return false
		}
		switch key {
		case "message", "_msg":
			text, ok := value.(string)
			if !ok || message != "" {
				return false
			}
			message = text
		case "level":
			text, ok := value.(string)
			if !ok || level != "" {
				return false
			}
			level = text
		case "height":
			parsed, ok := jsonInteger(value)
			if !ok || height != nil {
				return false
			}
			height = &parsed
		case "time":
			if _, timestamp := value.(string); timestamp {
				continue
			}
			parsed, ok := jsonInteger(value)
			if !ok || haltTime != nil {
				return false
			}
			haltTime = &parsed
		}
	}
	if _, err := decoder.Token(); err != nil {
		return false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return false
	}
	return level == "info" && message == sdkHaltMessage && height != nil && *height == haltHeight && haltTime != nil && *haltTime == 0
}

func jsonInteger(value any) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	parsed, err := strconv.ParseInt(number.String(), 10, 64)
	return parsed, err == nil
}

func validPlainSDKHaltLine(line string, haltHeight int64) bool {
	match := plainSDKHaltLine.FindStringIndex(line)
	if match == nil {
		return false
	}
	fields := strings.Fields(line[match[1]:])
	var height, haltTime *int64
	for _, field := range fields {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			return false
		}
		switch key {
		case "height":
			if height != nil {
				return false
			}
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return false
			}
			height = &parsed
		case "time":
			if haltTime != nil {
				return false
			}
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return false
			}
			haltTime = &parsed
		}
	}
	return height != nil && *height == haltHeight && haltTime != nil && *haltTime == 0
}
