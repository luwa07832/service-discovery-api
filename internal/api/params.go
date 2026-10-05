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
	// Path parameters are deliberately not merged into the bag. On path-style
	// entries they are resolved through resolveLocator so they always win; on
	// parameter-style entries no path parameter exists and merging would let a
	// matched path fragment masquerade as a request parameter.
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

// readJSONObject requires the request body to be a single JSON object. An
// empty body, a JSON array or any non-object payload is a parameter error.
// It is used by the batch entry points whose contract forbids merging query
// parameters or accepting a bare value.
func readJSONObject(c *gin.Context) (map[string]any, *apiError) {
	if c.Request.Body == nil {
		return nil, errInvalidParameter("request body must be a JSON object")
	}
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return nil, errInvalidParameter("request body cannot be read")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errInvalidParameter("request body must be a JSON object")
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil || body == nil {
		return nil, errInvalidParameter("request body must be a JSON object")
	}
	return body, nil
}

func stringValue(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

// readHealthyInput accepts a new healthy state for one instance from either a
// JSON body field ("healthy": true|false) or the ?healthy=true|false query
// parameter. The JSON field wins when both carry it. JSON accepts only
// booleans, while the query parameter accepts only the lowercase text "true"
// or "false"; every other representation is a parameter error.
func readHealthyInput(c *gin.Context) (bool, *apiError) {
	queryValue, queryPresent := c.GetQuery("healthy")

	if c.Request.Body != nil {
		raw, err := io.ReadAll(c.Request.Body)
		if err != nil {
			return false, errInvalidParameter("request body cannot be read")
		}
		if len(bytes.TrimSpace(raw)) > 0 {
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil || body == nil {
				return false, errInvalidParameter("request body must be a JSON object")
			}
			if value, present := body["healthy"]; present {
				healthy, ok := strictBoolValue(value)
				if !ok {
					return false, errInvalidParameter("healthy must be a boolean true or false")
				}
				return healthy, nil
			}
		}
	}

	if !queryPresent {
		return false, errInvalidParameter("healthy must be a boolean true or false")
	}
	switch queryValue {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, errInvalidParameter("healthy must be a boolean true or false")
	}
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

// Accepted timestamps cover [0000-01-01T00:00:00Z, 10000-01-01T00:00:00Z)
// once converted to UTC: the lower bound is included, the upper bound is
// excluded. These are exactly the instants the RFC3339 storage format can
// write and read back, so anything outside could never round-trip.
var (
	minAcceptedTime = time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC)
	maxAcceptedTime = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
)

func inAcceptedTimeRange(t time.Time) bool {
	t = t.UTC()
	return !t.Before(minAcceptedTime) && t.Before(maxAcceptedTime)
}

func parseTimeValue(value any) (time.Time, bool) {
	switch typed := value.(type) {
	case string:
		text := strings.TrimSpace(typed)
		if text == "" {
			return time.Time{}, false
		}
		if number, err := strconv.ParseFloat(text, 64); err == nil {
			return unixTimeInRange(number)
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
			if parsed, err := time.Parse(layout, text); err == nil {
				if !inAcceptedTimeRange(parsed) {
					return time.Time{}, false
				}
				return parsed, true
			}
		}
	case float64:
		return unixTimeInRange(typed)
	}
	return time.Time{}, false
}

// unixTimeInRange converts a Unix second count that lands inside the
// accepted range. The range check runs on the float itself, before any int64
// conversion, so non-finite values (NaN, Inf, Infinity with any sign) and
// out-of-range finite values are rejected instead of overflowing and
// wrapping back into a valid-looking date.
func unixTimeInRange(seconds float64) (time.Time, bool) {
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) ||
		seconds < float64(minAcceptedTime.Unix()) || seconds >= float64(maxAcceptedTime.Unix()) {
		return time.Time{}, false
	}
	return unixTime(seconds), true
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
