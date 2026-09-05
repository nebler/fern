package backgroundroute

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
)

const (
	maxAttachmentProjectionBytes = 4 << 20
	attachmentWorkspace          = "/home/user/workspace"
)

type attachmentSessionContextKey struct{}

var attachmentReadPaths = map[string]struct{}{
	"/api/health":                    {},
	"/path":                          {},
	"/project/current":               {},
	"/provider":                      {},
	"/experimental/console":          {},
	"/global/event":                  {},
	"/config":                        {},
	"/config/providers":              {},
	"/agent":                         {},
	"/experimental/capabilities":     {},
	"/api/location":                  {},
	"/api/skill":                     {},
	"/api/integration":               {},
	"/api/reference":                 {},
	"/api/agent":                     {},
	"/api/provider":                  {},
	"/api/command":                   {},
	"/api/model":                     {},
	"/mcp":                           {},
	"/command":                       {},
	"/experimental/resource":         {},
	"/lsp":                           {},
	"/formatter":                     {},
	"/provider/auth":                 {},
	"/vcs":                           {},
	"/find":                          {},
	"/find/file":                     {},
	"/find/symbol":                   {},
	"/file":                          {},
	"/file/content":                  {},
	"/file/status":                   {},
	"/experimental/workspace":        {},
	"/experimental/workspace/status": {},
	"/session":                       {},
	"/session/status":                {},
	"/api/session":                   {},
	"/api/session/active":            {},
	"/api/event":                     {},
}

func attachmentReadAllowed(path string, query url.Values, sessionID string) bool {
	if path == "/file" || path == "/file/content" {
		values := query["path"]
		if len(values) != 1 || strings.Contains(values[0], "\\") || pathpkgEscapesWorkspace(values[0]) {
			return false
		}
	}
	if _, ok := attachmentReadPaths[path]; ok {
		if path == "/api/session" {
			for _, parent := range query["parentID"] {
				if parent != sessionID {
					return false
				}
			}
		}
		return true
	}
	if strings.HasPrefix(path, "/project/") && strings.HasSuffix(path, "/directories") {
		middle := strings.TrimSuffix(strings.TrimPrefix(path, "/project/"), "/directories")
		return middle != "" && !strings.Contains(middle, "/")
	}
	for _, prefix := range []string{"/session/" + sessionID, "/api/session/" + sessionID, "/api/experimental/session/" + sessionID} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

func attachmentWorkspaceSelectionAllowed(request *http.Request) bool {
	values := request.Header.Values("X-OpenCode-Directory")
	if len(values) > 1 || len(values) == 1 && values[0] != attachmentWorkspace {
		return false
	}
	query := request.URL.Query()
	if len(query["workspace"]) != 0 {
		return false
	}
	for _, key := range []string{"directory", "location[directory]"} {
		for _, value := range query[key] {
			if value != attachmentWorkspace {
				return false
			}
		}
	}
	if request.URL.Path == "/session" {
		for _, value := range query["path"] {
			if value != strings.TrimPrefix(attachmentWorkspace, "/") {
				return false
			}
		}
	}
	return true
}

func pathpkgEscapesWorkspace(value string) bool {
	return path.IsAbs(value) || path.Clean(value) == ".." || strings.HasPrefix(path.Clean(value), "../")
}

func filterAttachmentResponse(response *http.Response, sessionID string) error {
	if sessionID == "" || response == nil || response.Request == nil || response.Body == nil {
		return errors.New("attachment response is missing its session fence")
	}
	switch response.Request.URL.Path {
	case "/global/event", "/api/event":
		response.Body = filterAttachmentEvents(response.Body, sessionID)
		response.ContentLength = -1
		response.Header.Del("Content-Length")
		return nil
	case "/session":
		return filterJSONArrayResponse(response, sessionID)
	case "/session/status":
		return filterJSONObjectResponse(response, sessionID, false)
	case "/api/session":
		return filterAPIDataResponse(response, sessionID, false)
	case "/api/session/active":
		return filterAPIDataResponse(response, sessionID, true)
	default:
		return nil
	}
}

func readProjection(response *http.Response) ([]byte, error) {
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxAttachmentProjectionBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxAttachmentProjectionBytes {
		return nil, errors.New("attachment projection exceeds limit")
	}
	return data, nil
}

func replaceProjection(response *http.Response, data []byte) {
	response.Body = io.NopCloser(bytes.NewReader(data))
	response.ContentLength = int64(len(data))
	response.Header.Set("Content-Length", fmt.Sprint(len(data)))
}

func filterJSONArrayResponse(response *http.Response, sessionID string) error {
	data, err := readProjection(response)
	if err != nil {
		return err
	}
	var values []json.RawMessage
	if err := json.Unmarshal(data, &values); err != nil {
		return errors.New("invalid OpenCode session list")
	}
	filtered := filterSessions(values, sessionID)
	data, err = json.Marshal(filtered)
	if err != nil {
		return err
	}
	replaceProjection(response, data)
	return nil
}

func filterJSONObjectResponse(response *http.Response, sessionID string, nested bool) error {
	data, err := readProjection(response)
	if err != nil {
		return err
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(data, &values); err != nil {
		return errors.New("invalid OpenCode session status")
	}
	if nested {
		return errors.New("unexpected nested session projection")
	}
	for id := range values {
		if id != sessionID {
			delete(values, id)
		}
	}
	data, err = json.Marshal(values)
	if err != nil {
		return err
	}
	replaceProjection(response, data)
	return nil
}

func filterAPIDataResponse(response *http.Response, sessionID string, object bool) error {
	data, err := readProjection(response)
	if err != nil {
		return err
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		return errors.New("invalid OpenCode API session projection")
	}
	if object {
		var values map[string]json.RawMessage
		if err := json.Unmarshal(envelope["data"], &values); err != nil {
			return errors.New("invalid OpenCode active-session projection")
		}
		for id := range values {
			if id != sessionID {
				delete(values, id)
			}
		}
		envelope["data"], err = json.Marshal(values)
	} else {
		var values []json.RawMessage
		if err := json.Unmarshal(envelope["data"], &values); err != nil {
			return errors.New("invalid OpenCode API session list")
		}
		envelope["data"], err = json.Marshal(filterSessions(values, sessionID))
	}
	if err != nil {
		return err
	}
	data, err = json.Marshal(envelope)
	if err != nil {
		return err
	}
	replaceProjection(response, data)
	return nil
}

func filterSessions(values []json.RawMessage, sessionID string) []json.RawMessage {
	filtered := make([]json.RawMessage, 0, 1)
	for _, value := range values {
		var identity struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(value, &identity) == nil && identity.ID == sessionID {
			filtered = append(filtered, value)
		}
	}
	return filtered
}

func filterAttachmentEvents(body io.ReadCloser, sessionID string) io.ReadCloser {
	reader, writer := io.Pipe()
	go func() {
		defer body.Close()
		scanner := bufio.NewScanner(body)
		scanner.Buffer(make([]byte, 64<<10), maxAttachmentProjectionBytes)
		var event bytes.Buffer
		flush := func() error {
			if event.Len() == 0 {
				return nil
			}
			data := event.Bytes()
			if attachmentEventAllowed(data, sessionID) {
				if _, err := writer.Write(data); err != nil {
					return err
				}
			}
			event.Reset()
			return nil
		}
		for scanner.Scan() {
			line := scanner.Bytes()
			event.Write(line)
			event.WriteByte('\n')
			if len(line) == 0 {
				if err := flush(); err != nil {
					_ = writer.CloseWithError(err)
					return
				}
			}
		}
		if err := flush(); err != nil {
			_ = writer.CloseWithError(err)
			return
		}
		_ = writer.CloseWithError(scanner.Err())
	}()
	return reader
}

func attachmentEventAllowed(event []byte, sessionID string) bool {
	var payload bytes.Buffer
	for _, line := range bytes.Split(event, []byte{'\n'}) {
		if value, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			payload.Write(bytes.TrimSpace(value))
		}
	}
	if payload.Len() == 0 {
		return true
	}
	var value any
	if json.Unmarshal(payload.Bytes(), &value) != nil {
		return false
	}
	seen, foreign := sessionIdentities(value, sessionID, "")
	if foreign {
		return false
	}
	root, _ := value.(map[string]any)
	eventType, _ := root["type"].(string)
	for _, prefix := range []string{"session.", "message.", "permission.", "question.", "todo."} {
		if strings.HasPrefix(eventType, prefix) {
			return seen
		}
	}
	return true
}

func sessionIdentities(value any, sessionID, key string) (bool, bool) {
	seen := false
	switch typed := value.(type) {
	case map[string]any:
		for childKey, child := range typed {
			childSeen, foreign := sessionIdentities(child, sessionID, childKey)
			seen = seen || childSeen
			if foreign {
				return seen, true
			}
		}
	case []any:
		for _, child := range typed {
			childSeen, foreign := sessionIdentities(child, sessionID, key)
			seen = seen || childSeen
			if foreign {
				return seen, true
			}
		}
	case string:
		identityKey := key == "session" || key == "sessionID" || key == "sessionId" || key == "session_id" ||
			key == "parentID" || key == "parentId" || key == "parent_id" || key == "id"
		if identityKey && strings.HasPrefix(typed, "ses_") {
			return true, typed != sessionID
		}
	}
	return seen, false
}
