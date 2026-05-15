package sandbox

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// validateInput runs schema-driven validation over a flat input map, returning
// a cleaned data map (only fields the schema declares) and any per-field
// errors. Mirrors Rails strong-parameters posture: unknown fields are dropped
// silently so the JS handler can pass `request.form` or `request.json`
// without worrying about extras. Missing maxLen on string types defaults to
// defaultStringMaxLen so a forgotten cap still has a bound.
//
// Supported types: "string", "email", "url", "integer", "number", "boolean".
//
// The Go side does not panic on malformed schemas; instead it surfaces a
// validation error for the offending field so the LLM author sees the issue
// in the response and can fix it.
func validateInput(input map[string]any, schema map[string]any) (map[string]any, []validationError) {
	data := make(map[string]any, len(schema))
	var errs []validationError
	for field, raw := range schema {
		fieldSchema, ok := raw.(map[string]any)
		if !ok {
			errs = append(errs, validationError{Field: field, Message: "schema entry must be an object"})
			continue
		}
		clean, ferr := validateField(field, input[field], fieldSchema)
		if ferr != nil {
			errs = append(errs, *ferr)
			continue
		}
		if clean != nil {
			data[field] = clean
		}
	}
	return data, errs
}

type validationError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

const (
	defaultStringMaxLen = 1024
	maxValidatableLen   = 64 * 1024 // bytes; over this we reject before coercion
)

var (
	emailPattern = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)
)

// validateField returns the cleaned value (nil if absent and not required) or
// a single validation error. Splitting per-field keeps validateInput small
// enough to stay under the cyclomatic limit.
func validateField(field string, raw any, schema map[string]any) (any, *validationError) {
	required := boolField(schema, "required")
	if raw == nil {
		if required {
			return nil, &validationError{Field: field, Message: "required"}
		}
		return nil, nil
	}
	typeName, _ := schema["type"].(string)
	if typeName == "" {
		typeName = "string"
	}
	switch typeName {
	case "string":
		return validateString(field, raw, schema, required)
	case "email":
		return validateEmail(field, raw, schema, required)
	case "url":
		return validateURL(field, raw, schema, required)
	case "integer":
		return validateInteger(field, raw, schema)
	case "number":
		return validateNumber(field, raw, schema)
	case "boolean":
		return validateBoolean(field, raw, required)
	default:
		return nil, &validationError{Field: field, Message: "unknown type " + typeName}
	}
}

func validateString(field string, raw any, schema map[string]any, required bool) (any, *validationError) {
	s, ok := coerceString(raw)
	if !ok {
		return nil, &validationError{Field: field, Message: "must be a string"}
	}
	if boolField(schema, "trim") {
		s = strings.TrimSpace(s)
	}
	if s == "" {
		if required {
			return nil, &validationError{Field: field, Message: "required"}
		}
		return nil, nil
	}
	maxLen := intField(schema, "maxLen", defaultStringMaxLen)
	if maxLen > 0 && len(s) > maxLen {
		return nil, &validationError{Field: field, Message: fmt.Sprintf("must be at most %d characters", maxLen)}
	}
	minLen := intField(schema, "minLen", 0)
	if minLen > 0 && len(s) < minLen {
		return nil, &validationError{Field: field, Message: fmt.Sprintf("must be at least %d characters", minLen)}
	}
	if pat, ok := schema["pattern"].(string); ok && pat != "" {
		re, err := regexp.Compile(pat)
		if err != nil {
			return nil, &validationError{Field: field, Message: "schema pattern is invalid: " + err.Error()}
		}
		if !re.MatchString(s) {
			return nil, &validationError{Field: field, Message: "format is invalid"}
		}
	}
	return s, nil
}

func validateEmail(field string, raw any, schema map[string]any, required bool) (any, *validationError) {
	clean, err := validateString(field, raw, schema, required)
	if err != nil {
		return nil, err
	}
	if clean == nil {
		return nil, nil
	}
	s := clean.(string)
	if !emailPattern.MatchString(s) {
		return nil, &validationError{Field: field, Message: "must be a valid email"}
	}
	return s, nil
}

func validateURL(field string, raw any, schema map[string]any, required bool) (any, *validationError) {
	clean, err := validateString(field, raw, schema, required)
	if err != nil {
		return nil, err
	}
	if clean == nil {
		return nil, nil
	}
	s := clean.(string)
	u, perr := url.Parse(s)
	if perr != nil || u.Scheme == "" || u.Host == "" {
		return nil, &validationError{Field: field, Message: "must be a valid URL"}
	}
	return s, nil
}

func validateInteger(field string, raw any, schema map[string]any) (any, *validationError) {
	n, err := coerceInteger(raw)
	if err != nil {
		return nil, &validationError{Field: field, Message: "must be an integer"}
	}
	if v, ok := intLookup(schema, "min"); ok && n < v {
		return nil, &validationError{Field: field, Message: fmt.Sprintf("must be at least %d", v)}
	}
	if v, ok := intLookup(schema, "max"); ok && n > v {
		return nil, &validationError{Field: field, Message: fmt.Sprintf("must be at most %d", v)}
	}
	return n, nil
}

func validateNumber(field string, raw any, schema map[string]any) (any, *validationError) {
	f, err := coerceFloat(raw)
	if err != nil {
		return nil, &validationError{Field: field, Message: "must be a number"}
	}
	if v, ok := floatLookup(schema, "min"); ok && f < v {
		return nil, &validationError{Field: field, Message: fmt.Sprintf("must be at least %v", v)}
	}
	if v, ok := floatLookup(schema, "max"); ok && f > v {
		return nil, &validationError{Field: field, Message: fmt.Sprintf("must be at most %v", v)}
	}
	return f, nil
}

func validateBoolean(field string, raw any, required bool) (any, *validationError) {
	b, ok := coerceBoolean(raw)
	if !ok {
		return nil, &validationError{Field: field, Message: "must be a boolean"}
	}
	if required && !b {
		return nil, &validationError{Field: field, Message: "required"}
	}
	return b, nil
}

func coerceString(raw any) (string, bool) {
	switch v := raw.(type) {
	case string:
		if len(v) > maxValidatableLen {
			return "", false
		}
		return v, true
	case int64:
		return strconv.FormatInt(v, 10), true
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	case bool:
		if v {
			return "true", true
		}
		return "false", true
	default:
		return "", false
	}
}

func coerceInteger(raw any) (int64, error) {
	switch v := raw.(type) {
	case int64:
		return v, nil
	case float64:
		if v != float64(int64(v)) {
			return 0, errors.New("not an integer")
		}
		return int64(v), nil
	case string:
		s := strings.TrimSpace(v)
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, errors.New("not an integer")
		}
		return n, nil
	default:
		return 0, errors.New("not an integer")
	}
}

func coerceFloat(raw any) (float64, error) {
	switch v := raw.(type) {
	case float64:
		return v, nil
	case int64:
		return float64(v), nil
	case string:
		s := strings.TrimSpace(v)
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, errors.New("not a number")
		}
		return f, nil
	default:
		return 0, errors.New("not a number")
	}
}

func coerceBoolean(raw any) (bool, bool) {
	switch v := raw.(type) {
	case bool:
		return v, true
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "on", "yes", "1":
			return true, true
		case "false", "off", "no", "0", "":
			return false, true
		}
		return false, false
	case int64:
		return v != 0, true
	case float64:
		return v != 0, true
	default:
		return false, false
	}
}

func boolField(schema map[string]any, key string) bool {
	v, _ := schema[key].(bool)
	return v
}

func intField(schema map[string]any, key string, def int) int {
	if n, ok := intLookup(schema, key); ok {
		return int(n)
	}
	return def
}

func intLookup(schema map[string]any, key string) (int64, bool) {
	switch v := schema[key].(type) {
	case int64:
		return v, true
	case float64:
		return int64(v), true
	default:
		return 0, false
	}
}

func floatLookup(schema map[string]any, key string) (float64, bool) {
	switch v := schema[key].(type) {
	case float64:
		return v, true
	case int64:
		return float64(v), true
	default:
		return 0, false
	}
}
