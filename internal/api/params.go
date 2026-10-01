package api

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Parameter names accepted on every entry point. The first name of each list
// is the canonical name used in responses.
var (
	serviceNameKeys = []string{"service_name", "serviceName", "service"}
	instanceIDKeys  = []string{"instance_id", "instanceId", "instance", "id"}
	hostKeys        = []string{"host", "hostname", "host_address"}
	portKeys        = []string{"port"}
	addressKeys     = []string{"address", "addr", "endpoint"}
	requestAtKeys   = []string{"request_at", "request_time", "requested_at", "now", "at"}
	healthKeys      = []string{"health", "healthy", "status", "state"}
	weightKeys      = []string{"weight"}
	heartbeatKeys   = []string{"heartbeat_at", "heartbeatAt", "heartbeat", "heartbeat_time", "heartbeatTime", "last_heartbeat"}
	evaluateAtKeys  = []string{"evaluate_at", "evaluateAt", "at", "now", "evaluate_time", "evaluateTime"}
	timeoutKeys     = []string{"heartbeat_timeout", "heartbeatTimeout", "timeout", "timeout_seconds", "timeoutSeconds", "heartbeat_timeout_seconds"}
)

type apiError struct {
	status  int
	code    string
	message string
}

func errInvalidParameter(message string) *apiError {
	return &apiError{http.StatusBadRequest, "INVALID_ARGUMENT", message}
}

func errInstanceNotFound() *apiError {
	return &apiError{http.StatusNotFound, "instance_not_found", "service instance not found"}
}

func errStorageUnavailable() *apiError {
	return &apiError{http.StatusServiceUnavailable, "storage_unavailable", "database is not available"}
}

func writeError(c *gin.Context, e *apiError) {
	c.JSON(e.status, gin.H{"error": gin.H{"code": e.code, "message": e.message}})
}

// paramBag merges path parameters, JSON body fields and query parameters.
// Explicit path parameters win, then body fields, then query parameters.
type paramBag struct {
	values map[string]any
}

func buildParamBag(c *gin.Context) (*paramBag, *apiError) {
	bag := &paramBag{values: make(map[string]any)}

	for key, values := range c.Request.URL.Query() {
		if len(values) > 0 {
			bag.values[key] = values[0]
		}
	}

	if c.Request.Body != nil {
		raw, err := io.ReadAll(c.Request.Body)
		if err != nil {
			return nil, errInvalidParameter("request body cannot be read")
		}
		if len(bytes.TrimSpace(raw)) > 0 {
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				return nil, errInvalidParameter("request body must be a JSON object")
			}
			for key, value := range body {
				bag.values[key] = value
			}
		}
	}

	for _, param := range c.Params {
		bag.values[param.Key] = param.Value
	}
	return bag, nil
}

func (b *paramBag) get(keys []string) (any, bool) {
	for _, key := range keys {
		if value, ok := b.values[key]; ok {
			return value, true
		}
	}
	return nil, false
}

func stringValue(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

// requiredText returns the trimmed text for the first matching key, or an error
// when the value is missing or blank.
func (b *paramBag) requiredText(keys []string, message string) (string, *apiError) {
	value, ok := b.get(keys)
	if !ok {
		return "", errInvalidParameter(message)
	}
	text := strings.TrimSpace(stringValue(value))
	if text == "" {
		return "", errInvalidParameter(message)
	}
	return text, nil
}

// positiveNumber accepts a finite number strictly greater than zero. A zero
// or negative weight is an invalid argument and leaves the record untouched.
func positiveNumber(value any) (float64, bool) {
	number, ok := toFloat(value)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || number <= 0 {
		return 0, false
	}
	return number, true
}

// portValue parses a TCP port: an integer in the valid port range.
func portValue(value any) (int, bool) {
	number, ok := toFloat(value)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) {
		return 0, false
	}
	if number < 0 || number > 65535 || number != math.Trunc(number) {
		return 0, false
	}
	return int(number), true
}

// splitHostPort extracts the host and port from a "host:port" address. It is
// used only as a fallback when a request carries the legacy address field.
func splitHostPort(address string) (string, int, bool) {
	host, portText, ok := strings.Cut(strings.TrimSpace(address), ":")
	if !ok || host == "" {
		return "", 0, false
	}
	port, err := strconv.Atoi(strings.TrimSpace(portText))
	if err != nil || port < 0 || port > 65535 {
		return "", 0, false
	}
	return host, port, true
}

func toFloat(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		number, err := typed.Float64()
		if err != nil {
			return 0, false
		}
		return number, true
	case string:
		number, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if err != nil {
			return 0, false
		}
		return number, true
	default:
		return 0, false
	}
}

func parseHealthValue(value any) (bool, bool) {
	// Only an explicit boolean state is accepted: JSON true/false in bodies
	// and the text "true"/"false" in query parameters. Any other value
	// (numbers, "healthy"/"unhealthy" text, etc.) is an invalid argument so
	// different implementations cannot disagree on the resulting state.
	if typed, ok := value.(bool); ok {
		return typed, true
	}
	if typed, ok := value.(string); ok {
		switch strings.TrimSpace(typed) {
		case "true":
			return true, true
		case "false":
			return false, true
		}
	}
	return false, false
}

func parseTimeValue(value any) (time.Time, bool) {
	switch typed := value.(type) {
	case string:
		text := strings.TrimSpace(typed)
		if text == "" {
			return time.Time{}, false
		}
		if number, err := strconv.ParseFloat(text, 64); err == nil {
			return unixTime(number), true
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
			if parsed, err := time.Parse(layout, text); err == nil {
				return parsed, true
			}
		}
	case float64:
		return unixTime(typed), true
	}
	return time.Time{}, false
}

func unixTime(seconds float64) time.Time {
	whole := int64(seconds)
	fraction := seconds - float64(whole)
	return time.Unix(whole, int64(fraction*1e9)).UTC()
}

// parseTimeout accepts a number of seconds or a duration string such as "30s".
// A non-positive duration is reported as invalid by the caller.
func parseTimeout(value any) (time.Duration, bool) {
	switch typed := value.(type) {
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return 0, false
		}
		return time.Duration(typed * float64(time.Second)), true
	case string:
		text := strings.TrimSpace(typed)
		if text == "" {
			return 0, false
		}
		if duration, err := time.ParseDuration(text); err == nil {
			return duration, true
		}
		if number, err := strconv.ParseFloat(text, 64); err == nil && !math.IsNaN(number) && !math.IsInf(number, 0) {
			return time.Duration(number * float64(time.Second)), true
		}
	}
	return 0, false
}
