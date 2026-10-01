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
	addressKeys     = []string{"address", "addr", "host", "endpoint"}
	portKeys        = []string{"port"}
	healthKeys      = []string{"health", "healthy", "status", "state"}
	weightKeys      = []string{"weight"}
	heartbeatKeys   = []string{"heartbeat_at", "heartbeatAt", "heartbeat", "heartbeat_time", "heartbeatTime", "last_heartbeat"}
	evaluateAtKeys  = []string{"evaluate_at", "evaluateAt", "at", "now", "evaluate_time", "evaluateTime"}
	timeoutKeys     = []string{"heartbeat_timeout", "heartbeatTimeout", "timeout", "timeout_seconds", "timeoutSeconds", "heartbeat_timeout_seconds"}
	instancesKeys   = []string{"instances"}
)

type apiError struct {
	status  int
	code    string
	message string
}

func errInvalidParameter(message string) *apiError {
	return &apiError{http.StatusBadRequest, "invalid_parameter", message}
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

// requiredHeartbeat reads a mandatory heartbeat timestamp, accepting the same
// RFC3339 or Unix-second forms as the registration entry.
func (b *paramBag) requiredHeartbeat() (time.Time, *apiError) {
	value, ok := b.get(heartbeatKeys)
	if !ok {
		return time.Time{}, errInvalidParameter("heartbeat_at is required")
	}
	parsed, ok := parseTimeValue(value)
	if !ok {
		return time.Time{}, errInvalidParameter("heartbeat_at must be a valid timestamp")
	}
	return parsed, nil
}

// positiveNumber accepts weights: only numbers strictly greater than zero.
func positiveNumber(value any) (float64, bool) {
	number, ok := toFloat(value)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || number <= 0 {
		return 0, false
	}
	return number, true
}

// parsePortValue accepts a non-negative integer port number.
func parsePortValue(value any) (int64, bool) {
	number, ok := toFloat(value)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || number < 0 ||
		number != math.Trunc(number) || number > math.MaxInt64 {
		return 0, false
	}
	return int64(number), true
}

// strictBoolValue accepts only JSON boolean true or false. Every other
// representation (text, numbers) is rejected so all implementations agree.
func strictBoolValue(value any) (bool, bool) {
	typed, ok := value.(bool)
	return typed, ok
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
	switch typed := value.(type) {
	case bool:
		return typed, true
	case float64:
		switch typed {
		case 1:
			return true, true
		case 0:
			return false, true
		}
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "healthy", "up", "on", "true", "1", "yes":
			return true, true
		case "unhealthy", "down", "off", "false", "0", "no":
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
