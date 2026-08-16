package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

func readEnvFile(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read environment file %q: %w", path, err)
	}
	defer file.Close()
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
		if len(value) >= 2 && (value[0] == '\'' && value[len(value)-1] == '\'' || value[0] == '"' && value[len(value)-1] == '"') {
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

func mergeEnvironment(configured map[string]string, fileValues map[string]string) map[string]string {
	merged := make(map[string]string, len(configured)+len(fileValues))
	for key, value := range configured {
		merged[key] = value
	}
	for key, value := range fileValues {
		if _, exists := merged[key]; !exists {
			merged[key] = value
		}
	}
	return merged
}
