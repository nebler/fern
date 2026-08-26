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
	generator, err := NewGenerator(bytes.NewReader(make([]byte, 256)), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	checks := []func() error{
		func() error {
			value, err := generator.WorkspaceID()
			if err == nil {
				_, err = ParseWorkspaceID(string(value))
			}
			return err
		},
		func() error {
			value, err := generator.TaskID()
			if err == nil {
				_, err = ParseTaskID(string(value))
			}
			return err
		},
		func() error {
			value, err := generator.AttemptID()
			if err == nil {
				_, err = ParseAttemptID(string(value))
			}
			return err
		},
		func() error {
			value, err := generator.ReceiptID()
			if err == nil {
				_, err = ParseReceiptID(string(value))
			}
			return err
		},
		func() error {
			value, err := generator.EventID()
			if err == nil {
				_, err = ParseEventID(string(value))
			}
			return err
		},
		func() error {
			value, err := generator.ApprovalID()
			if err == nil {
				_, err = ParseApprovalID(string(value))
			}
			return err
		},
		func() error {
			value, err := generator.SealRequestID()
			if err == nil {
				_, err = ParseSealRequestID(string(value))
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
			value, err := generator.VerificationID()
			if err == nil {
				_, err = ParseVerificationID(string(value))
			}
			return err
		},
		func() error {
			value, err := generator.PublicationOperationID()
			if err == nil {
				_, err = ParsePublicationOperationID(string(value))
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
	generator, err := NewGenerator(bytes.NewReader(make([]byte, 128)), func() time.Time { return time.UnixMilli(1_700_000_000_000) })
	if err != nil {
		t.Fatal(err)
	}
	ids, err := generator.GenerateAdmissionIDs()
	if err != nil {
		t.Fatal(err)
	}
	checks := []error{}
	_, err = ParseTaskID(string(ids.TaskID))
	checks = append(checks, err)
	_, err = ParseAttemptID(string(ids.AttemptID))
	checks = append(checks, err)
	_, err = ParseReceiptID(string(ids.ReceiptID))
	checks = append(checks, err)
	_, err = ParseEventID(string(ids.TaskEventID))
	checks = append(checks, err)
	_, err = ParseEventID(string(ids.AttemptEventID))
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
	if ids.TaskEventID == ids.AttemptEventID {
		t.Fatal("admission event IDs are equal")
	}
}

func TestGenerateSealRequestIDsReturnsCompleteValidatedSet(t *testing.T) {
	generator, err := NewGenerator(bytes.NewReader(make([]byte, 128)), func() time.Time { return time.UnixMilli(1_700_000_000_000) })
	if err != nil {
		t.Fatal(err)
	}
	ids, err := generator.GenerateSealRequestIDs()
	if err != nil {
		t.Fatal(err)
	}
	validations := []error{}
	_, err = ParseSealRequestID(string(ids.SealRequestID))
	validations = append(validations, err)
	_, err = ParseReceiptID(string(ids.ReceiptID))
	validations = append(validations, err)
	_, err = ParseResultID(string(ids.ResultID))
	validations = append(validations, err)
	_, err = ParseEventID(string(ids.ResultEventID))
	validations = append(validations, err)
	_, err = ParseEventID(string(ids.TaskEventID))
	validations = append(validations, err)
	for _, validationErr := range validations {
		if validationErr != nil {
			t.Fatal(validationErr)
		}
	}
	if ids.ResultEventID == ids.TaskEventID {
		t.Fatal("seal event IDs are equal")
	}
}

func TestGenerateAdmissionIDsReturnsZeroSetOnLateEntropyFailure(t *testing.T) {
	generator, err := NewGenerator(io.LimitReader(bytes.NewReader(make([]byte, 128)), 30), func() time.Time { return time.UnixMilli(1_700_000_000_000) })
	if err != nil {
		t.Fatal(err)
	}
	ids, err := generator.GenerateAdmissionIDs()
	if ids != (AdmissionIDs{}) || !errors.Is(err, ErrIDGeneration) {
		t.Fatalf("late failure = %+v, %v", ids, err)
	}
}

func TestGeneratorUUIDv7IsMonotonicAcrossClockRegression(t *testing.T) {
	times := []time.Time{time.UnixMilli(2000), time.UnixMilli(2000), time.UnixMilli(1999), time.UnixMilli(2001)}
	index := 0
	generator, err := NewGenerator(bytes.NewReader(make([]byte, 64)), func() time.Time {
		value := times[index]
		index++
		return value
	})
	if err != nil {
		t.Fatal(err)
	}
	values := make([]string, len(times))
	for index := range values {
		value, err := generator.TaskID()
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
	generator, err := NewGenerator(bytes.NewReader(make([]byte, 32)), func() time.Time { return time.UnixMilli(3000) })
	if err != nil {
		t.Fatal(err)
	}
	const count = 500
	values := make([]string, count)
	var wait sync.WaitGroup
	for index := range count {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			value, generationErr := generator.EventID()
			if generationErr != nil {
				t.Errorf("EventID: %v", generationErr)
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
	if _, err := NewGenerator(nil, time.Now); !errors.Is(err, ErrIDGeneration) {
		t.Fatalf("nil entropy error = %v", err)
	}
	if _, err := NewGenerator(bytes.NewReader(nil), nil); !errors.Is(err, ErrIDGeneration) {
		t.Fatalf("nil clock error = %v", err)
	}
	generator, _ := NewGenerator(errorReader{}, func() time.Time { return time.UnixMilli(1) })
	if value, err := generator.TaskID(); value != "" || !errors.Is(err, ErrIDGeneration) {
		t.Fatalf("entropy failure = %q, %v", value, err)
	}
	if value, err := generator.OpenCodeMessageID(); value != "" || !errors.Is(err, ErrIDGeneration) {
		t.Fatalf("OpenCode entropy failure = %q, %v", value, err)
	}
	generator, _ = NewGenerator(bytes.NewReader(make([]byte, 16)), func() time.Time { return time.UnixMilli(-1) })
	if value, err := generator.TaskID(); value != "" || !errors.Is(err, ErrIDGeneration) {
		t.Fatalf("clock failure = %q, %v", value, err)
	}
}

func TestGeneratorAdvancesTimestampWhenRandomFieldOverflows(t *testing.T) {
	entropy := append(bytes.Repeat([]byte{0xff}, 10), make([]byte, 10)...)
	generator, err := NewGenerator(bytes.NewReader(entropy), func() time.Time { return time.UnixMilli(4000) })
	if err != nil {
		t.Fatal(err)
	}
	first, err := generator.TaskID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := generator.TaskID()
	if err != nil {
		t.Fatal(err)
	}
	if second <= first {
		t.Fatalf("overflow did not advance UUID: %q then %q", first, second)
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
