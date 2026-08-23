package taskstore

import (
	"context"
	"errors"
	"testing"

	"github.com/nebler/fern/internal/task"
)

func TestListTasksReturnsDurableLatestSnapshots(t *testing.T) {
	store := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = store.Close() })
	createTestWorkspace(t, store)
	first, err := store.AdmitTask(context.Background(), testAdmission(1600, "list-first", "first"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.AdmitTask(context.Background(), testAdmission(1601, "list-second", "second"))
	if err != nil {
		t.Fatal(err)
	}
	page, err := store.ListTasks(context.Background(), testWorkspaceID(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || page[0].Task.ID != second.Task.ID || page[0].Attempt.ID != second.Attempt.ID {
		t.Fatalf("page = %+v", page)
	}
	page, err = store.ListTasks(context.Background(), testWorkspaceID(), 2)
	if err != nil || len(page) != 2 || page[1].Task.ID != first.Task.ID {
		t.Fatalf("full page = %+v err=%v", page, err)
	}
}

func TestListTasksValidatesScopeAndLimit(t *testing.T) {
	store := openTestStore(t, testDBPath(t))
	t.Cleanup(func() { _ = store.Close() })
	for _, test := range []struct {
		workspace string
		limit     int
	}{
		{"bad", 1}, {string(testWorkspaceID()), 0}, {string(testWorkspaceID()), MaxTaskListLimit + 1},
	} {
		if _, err := store.ListTasks(context.Background(), task.WorkspaceID(test.workspace), test.limit); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("workspace=%q limit=%d err=%v", test.workspace, test.limit, err)
		}
	}
}
