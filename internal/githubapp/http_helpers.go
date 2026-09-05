package githubapp

import (
	"encoding/json"
	"net/url"
	"reflect"

	"github.com/nebler/fern/internal/strictjson"
)

const maxJSONDepth = 64

func validAPIBase(base string) bool {
	parsed, err := url.Parse(base)
	return err == nil && parsed.IsAbs() && parsed.Host != "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" && parsed.RawPath == "" && parsed.Path == ""
}

func decodeGitHubJSON(payload []byte, destination any) error {
	if err := strictjson.Check(payload, maxJSONDepth); err != nil {
		return ErrInvalidResponse
	}
	if err := json.Unmarshal(payload, destination); err != nil {
		return ErrInvalidResponse
	}
	return nil
}

func firstError(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}

// isNilInterface reports whether value is a nil interface or a typed nil
// pointer, map, slice, channel, or function.
func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
