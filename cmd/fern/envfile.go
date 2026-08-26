package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"syscall"
)

func readEnvFile(path string) (map[string]string, error) {
	descriptor, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open environment file %q: %w", path, err)
	}
	file := os.NewFile(uintptr(descriptor), path)
	if file == nil {
		_ = syscall.Close(descriptor)
		return nil, fmt.Errorf("open environment file %q", path)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect environment file %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("environment file %q must be a regular file", path)
	}
	if info.Mode().Perm()&0o027 != 0 {
		return nil, fmt.Errorf("environment file %q permissions %o expose secrets; use 0600 or 0640", path, info.Mode().Perm())
	}
	values := make(map[string]string)
	scanner := bufio.NewScanner(file)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		text = strings.TrimSpace(strings.TrimPrefix(text, "export "))
		key, value, ok := strings.Cut(text, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" || strings.ContainsAny(key, " \t\r\n=\x00") {
			return nil, fmt.Errorf("parse environment file %q line %d: expected NAME=value", path, line)
		}
		value = strings.TrimSpace(value)
		quoted := len(value) >= 2 && (value[0] == '\'' && value[len(value)-1] == '\'' || value[0] == '"' && value[len(value)-1] == '"')
		if quoted {
			value = value[1 : len(value)-1]
		}
		if strings.ContainsAny(value, "\x00\r\n") {
			return nil, fmt.Errorf("parse environment file %q line %d: invalid value", path, line)
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read environment file %q: %w", path, err)
	}
	return values, nil
}

func mergeWorkspaceEnvironment(configured map[string]string, fileValues map[string]string) map[string]string {
	merged := make(map[string]string, len(configured)+len(fileValues))
	for key, value := range configured {
		merged[key] = value
	}
	for _, key := range forwardedSecretKeys {
		value, present := fileValues[key]
		if !present {
			continue
		}
		if _, exists := merged[key]; !exists {
			merged[key] = value
		}
	}
	return merged
}

func environmentLookup(fileValues map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		if value, exists := fileValues[key]; exists {
			return value, true
		}
		return os.LookupEnv(key)
	}
}
