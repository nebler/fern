package task

import (
	"bytes"
	"errors"
	"io"
	"sort"
	"sync"
	"testing"
	"time"
)

func TestGeneratorProducesEveryTypedID(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)
	generator := newTestGenerator(bytes.NewReader(make([]byte, 256)), func() time.Time { return now })
	checks := []func() error{
		func() error {
			value, err := generator.WorkspaceID()
			if err == nil {
				_, err = ParseWorkspaceID(string(value))
			}
			return err
		},
		func() error {
			value, err := generator.RunID()
			if err == nil {
				_, err = ParseRunID(string(value))
			}
			return err
		},
		func() error {
			value, err := generator.ResultID()
			if err == nil {
				_, err = ParseResultID(string(value))
			}
			return err
		},
		func() error {
			value, err := generator.OpenCodeSessionID()
			if err == nil {
				_, err = ParseOpenCodeSessionID(string(value))
			}
			return err
		},
		func() error {
			value, err := generator.OpenCodeMessageID()
			if err == nil {
				_, err = ParseOpenCodeMessageID(string(value))
			}
			return err
		},
	}
	for index, check := range checks {
		if err := check(); err != nil {
			t.Fatalf("typed ID %d: %v", index, err)
		}
	}
}

func TestGenerateAdmissionIDsReturnsCompleteValidatedSet(t *testing.T) {
	generator := newTestGenerator(bytes.NewReader(make([]byte, 128)), func() time.Time { return time.UnixMilli(1_700_000_000_000) })
	ids, err := generator.GenerateAdmissionIDs()
	if err != nil {
		t.Fatal(err)
	}
	checks := []error{}
	_, err = ParseRunID(string(ids.RunID))
	checks = append(checks, err)
	_, err = ParseOpenCodeSessionID(string(ids.OpenCodeSessionID))
	checks = append(checks, err)
	_, err = ParseOpenCodeMessageID(string(ids.OpenCodeMessageID))
	checks = append(checks, err)
	for index, validationErr := range checks {
		if validationErr != nil {
			t.Fatalf("admission ID %d: %v", index, validationErr)
		}
	}
}

func TestGenerateAdmissionIDsReturnsZeroSetOnLateEntropyFailure(t *testing.T) {
	generator := newTestGenerator(io.LimitReader(bytes.NewReader(make([]byte, 128)), 30), func() time.Time { return time.UnixMilli(1_700_000_000_000) })
	ids, err := generator.GenerateAdmissionIDs()
	if ids != (AdmissionIDs{}) || !errors.Is(err, ErrIDGeneration) {
		t.Fatalf("late failure = %+v, %v", ids, err)
	}
}

func TestGeneratorUUIDv7IsMonotonicAcrossClockRegression(t *testing.T) {
	times := []time.Time{time.UnixMilli(2000), time.UnixMilli(2000), time.UnixMilli(1999), time.UnixMilli(2001)}
	index := 0
	generator := newTestGenerator(bytes.NewReader(make([]byte, 64)), func() time.Time {
		value := times[index]
		index++
		return value
	})
	values := make([]string, len(times))
	for index := range values {
		value, err := generator.RunID()
		if err != nil {
			t.Fatal(err)
		}
		values[index] = string(value)
		if index > 0 && values[index] <= values[index-1] {
			t.Fatalf("IDs not monotonic: %q then %q", values[index-1], values[index])
		}
	}
}

func TestGeneratorConcurrentIDsAreUnique(t *testing.T) {
	generator := newTestGenerator(bytes.NewReader(make([]byte, 32)), func() time.Time { return time.UnixMilli(3000) })
	const count = 500
	values := make([]string, count)
	var wait sync.WaitGroup
	for index := range count {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			value, generationErr := generator.ResultID()
			if generationErr != nil {
				t.Errorf("ResultID: %v", generationErr)
				return
			}
			values[index] = string(value)
		}(index)
	}
	wait.Wait()
	sort.Strings(values)
	for index := 1; index < len(values); index++ {
		if values[index] == values[index-1] {
			t.Fatalf("duplicate ID %q", values[index])
		}
	}
}

func TestGeneratorEntropyAndClockFailuresDoNotReturnIDs(t *testing.T) {
	generator := newTestGenerator(errorReader{}, func() time.Time { return time.UnixMilli(1) })
	if value, err := generator.RunID(); value != "" || !errors.Is(err, ErrIDGeneration) {
		t.Fatalf("entropy failure = %q, %v", value, err)
	}
	if value, err := generator.OpenCodeMessageID(); value != "" || !errors.Is(err, ErrIDGeneration) {
		t.Fatalf("OpenCode entropy failure = %q, %v", value, err)
	}
	generator = newTestGenerator(bytes.NewReader(make([]byte, 16)), func() time.Time { return time.UnixMilli(-1) })
	if value, err := generator.RunID(); value != "" || !errors.Is(err, ErrIDGeneration) {
		t.Fatalf("clock failure = %q, %v", value, err)
	}
}

func TestGeneratorAdvancesTimestampWhenRandomFieldOverflows(t *testing.T) {
	entropy := append(bytes.Repeat([]byte{0xff}, 10), make([]byte, 10)...)
	generator := newTestGenerator(bytes.NewReader(entropy), func() time.Time { return time.UnixMilli(4000) })
	first, err := generator.RunID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := generator.RunID()
	if err != nil {
		t.Fatal(err)
	}
	if second <= first {
		t.Fatalf("overflow did not advance UUID: %q then %q", first, second)
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

// newTestGenerator replaces the entropy source and clock of the secure generator.
func newTestGenerator(random io.Reader, now func() time.Time) *Generator {
	return &Generator{random: random, now: now}
}
