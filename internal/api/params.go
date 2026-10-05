package api

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"regexp"
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

// paramBag merges JSON body fields and query parameters, keeping route path
// parameters separate. Body fields win over query parameters; path
// parameters are read through locateText and win over both of them.
type paramBag struct {
	values map[string]any
	path   map[string]any
}

func buildParamBag(c *gin.Context) (*paramBag, *apiError) {
	bag := &paramBag{
		values: make(map[string]any),
		path:   make(map[string]any),
	}

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
			// UseNumber keeps every JSON number as its literal text so
			// integer fields (port) can be parsed exactly, without the
			// float64 rounding that json.Unmarshal would apply.
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.UseNumber()
			if err := decoder.Decode(&body); err != nil {
				return nil, errInvalidParameter("request body must be a JSON object")
			}
			if decoder.More() {
				return nil, errInvalidParameter("request body must be a JSON object")
			}
			for key, value := range body {
				bag.values[key] = value
			}
		}
	}

	for _, param := range c.Params {
		bag.path[param.Key] = param.Value
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

// locateText resolves one locator field (service name or instance id). A
// value carried by the route path always wins: a conflicting, empty or
// non-string value of the same field in the body or query string is ignored,
// and a blank path segment is a parameter error that cannot be filled from
// another source. Only when the route carries no such segment does the field
// fall back to the existing body/query lookup rules.
func (b *paramBag) locateText(pathKey string, keys []string, message string) (string, *apiError) {
	if raw, fromPath := b.path[pathKey]; fromPath {
		text := strings.TrimSpace(stringValue(raw))
		if text == "" {
			return "", errInvalidParameter(message)
		}
		return text, nil
	}
	return b.requiredText(keys, message)
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

// parsePortValue accepts a non-negative integer port number in the range
// [0, 9223372036854775807]. JSON numbers and numeric strings are parsed from
// their decimal text with exact integer semantics: values above 2^53 keep
// their exact value, fractional forms such as 8080.0 or 8.08e3 collapse to
// their integer value, and the int64 bound is enforced without rounding, so
// 9223372036854775808 is rejected instead of overflowing the conversion.
func parsePortValue(value any) (int64, bool) {
	switch typed := value.(type) {
	case json.Number:
		return parsePortText(typed.String())
	case string:
		return parsePortText(strings.TrimSpace(typed))
	case float64:
		// A float64 at or beyond 2^63 cannot be a valid port: the largest
		// exactly representable candidate below it is 2^63-1024, and the
		// int64 conversion of anything larger would overflow.
		if math.IsNaN(typed) || math.IsInf(typed, 0) || typed != math.Trunc(typed) ||
			typed < 0 || typed >= 9223372036854775808.0 {
			return 0, false
		}
		return int64(typed), true
	case int:
		return int64(typed), typed >= 0
	case int64:
		return typed, typed >= 0
	default:
		return 0, false
	}
}

// portNumberPattern is the decimal grammar a port may use: an integer or a
// fixed/scientific decimal with an optional sign.
var portNumberPattern = regexp.MustCompile(`^[+-]?([0-9]+(\.[0-9]*)?|\.[0-9]+)([eE][+-]?[0-9]+)?$`)

// parsePortText parses the exact integer value of a decimal number without
// going through float64. The mantissa is kept as a digit string and the
// exponent as a small int, so huge exponents are rejected by digit counting
// instead of materializing enormous integers.
func parsePortText(text string) (int64, bool) {
	// Fast path: a plain decimal integer, exact for the whole int64 range.
	if number, err := strconv.ParseInt(text, 10, 64); err == nil {
		return number, number >= 0
	}
	if !portNumberPattern.MatchString(text) {
		return 0, false
	}
	negative := strings.HasPrefix(text, "-")
	mantissa := strings.TrimLeft(text, "+-")
	exponent := 0
	if at := strings.IndexAny(mantissa, "eE"); at >= 0 {
		expText := mantissa[at+1:]
		mantissa = mantissa[:at]
		if digits := strings.TrimLeft(strings.TrimLeft(expText, "+-"), "0"); len(digits) > 6 {
			// |exponent| >= 10^6: the value is either far beyond the int64
			// bound or far below one; only an all-zero mantissa is still
			// exactly zero.
			if allZeroDigits(mantissa) {
				return 0, true
			}
			return 0, false
		}
		exponent, _ = strconv.Atoi(expText)
	}
	intPart, fracPart := mantissa, ""
	if at := strings.IndexByte(mantissa, '.'); at >= 0 {
		intPart, fracPart = mantissa[:at], mantissa[at+1:]
	}
	// value = digits * 10^k
	digits := intPart + fracPart
	k := exponent - len(fracPart)
	digits = strings.TrimLeft(digits, "0")
	for strings.HasSuffix(digits, "0") {
		digits = digits[:len(digits)-1]
		k++
	}
	if digits == "" {
		return 0, true // exact zero, in any form
	}
	if negative || k < 0 {
		// A negative port is out of range; a negative remaining exponent
		// leaves a fractional part, so the value is not an integer.
		return 0, false
	}
	if len(digits)+k > 19 {
		return 0, false
	}
	number, err := strconv.ParseInt(digits+strings.Repeat("0", k), 10, 64)
	if err != nil {
		return 0, false // 19 digits but beyond math.MaxInt64
	}
	return number, true
}

// allZeroDigits reports whether a mantissa (digits with an optional dot)
// denotes zero.
func allZeroDigits(mantissa string) bool {
	for _, r := range mantissa {
		if r != '0' && r != '.' {
			return false
		}
	}
	return true
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
	case json.Number:
		if number, err := typed.Float64(); err == nil {
			switch number {
			case 1:
				return true, true
			case 0:
				return false, true
			}
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
	case json.Number:
		number, err := typed.Float64()
		if err != nil {
			return time.Time{}, false
		}
		return unixTimeInRange(number)
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
	case json.Number:
		number, err := typed.Float64()
		if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
			return 0, false
		}
		return time.Duration(number * float64(time.Second)), true
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
