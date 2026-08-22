package task

import (
	"errors"
	"testing"
)

func TestTaskTransitions(t *testing.T) {
	testTransitions(t, allTaskStates, map[TaskState][]TaskState{
		TaskQueued:           {TaskRunning, TaskCancelRequested, TaskUncertain, TaskRecoveryRequired, TaskFailed},
		TaskRunning:          {TaskInputRequired, TaskCancelRequested, TaskCompleted, TaskFailed, TaskUncertain, TaskRecoveryRequired},
		TaskInputRequired:    {TaskRunning, TaskCancelRequested, TaskFailed, TaskUncertain, TaskRecoveryRequired},
		TaskCancelRequested:  {TaskCanceled, TaskUncertain, TaskRecoveryRequired},
		TaskUncertain:        {TaskQueued, TaskRunning, TaskInputRequired, TaskCancelRequested, TaskCompleted, TaskFailed, TaskCanceled, TaskRecoveryRequired},
		TaskRecoveryRequired: {TaskQueued, TaskRunning, TaskCancelRequested, TaskFailed, TaskCanceled},
	}, AllowTaskTransition)
	testTerminal(t, allTaskStates, map[TaskState]bool{TaskCompleted: true, TaskFailed: true, TaskCanceled: true}, func(s TaskState) bool { return s.Terminal() })
}

func TestAttemptTransitions(t *testing.T) {
	testTransitions(t, allAttemptStates, map[AttemptState][]AttemptState{
		AttemptPrepared:         {AttemptDelivering, AttemptCancelRequested, AttemptRecoveryRequired, AttemptFailed},
		AttemptDelivering:       {AttemptAdmitted, AttemptUncertain, AttemptCancelRequested, AttemptRecoveryRequired, AttemptFailed},
		AttemptAdmitted:         {AttemptRunning, AttemptInputRequired, AttemptCancelRequested, AttemptSucceeded, AttemptFailed, AttemptUncertain, AttemptRecoveryRequired},
		AttemptRunning:          {AttemptInputRequired, AttemptCancelRequested, AttemptSucceeded, AttemptFailed, AttemptUncertain, AttemptRecoveryRequired},
		AttemptInputRequired:    {AttemptRunning, AttemptCancelRequested, AttemptFailed, AttemptUncertain, AttemptRecoveryRequired},
		AttemptCancelRequested:  {AttemptCanceled, AttemptUncertain, AttemptRecoveryRequired},
		AttemptUncertain:        {AttemptPrepared, AttemptAdmitted, AttemptRunning, AttemptInputRequired, AttemptCancelRequested, AttemptSucceeded, AttemptFailed, AttemptCanceled, AttemptRecoveryRequired},
		AttemptRecoveryRequired: {AttemptPrepared, AttemptAdmitted, AttemptRunning, AttemptCancelRequested, AttemptFailed, AttemptCanceled, AttemptSuperseded},
	}, AllowAttemptTransition)
	testTerminal(t, allAttemptStates, map[AttemptState]bool{AttemptSucceeded: true, AttemptFailed: true, AttemptCanceled: true, AttemptSuperseded: true}, func(s AttemptState) bool { return s.Terminal() })
}

func TestApprovalTransitions(t *testing.T) {
	testTransitions(t, allApprovalStates, map[ApprovalState][]ApprovalState{
		ApprovalPending:          {ApprovalDecisionRecorded, ApprovalExpired, ApprovalCanceled, ApprovalRecoveryRequired},
		ApprovalDecisionRecorded: {ApprovalDelivering, ApprovalCanceled, ApprovalRecoveryRequired},
		ApprovalDelivering:       {ApprovalApplied, ApprovalRejected, ApprovalUncertain, ApprovalRecoveryRequired},
		ApprovalUncertain:        {ApprovalDelivering, ApprovalApplied, ApprovalRejected, ApprovalCanceled, ApprovalRecoveryRequired},
		ApprovalRecoveryRequired: {ApprovalDelivering, ApprovalRejected, ApprovalCanceled},
	}, AllowApprovalTransition)
	testTerminal(t, allApprovalStates, map[ApprovalState]bool{ApprovalApplied: true, ApprovalRejected: true, ApprovalExpired: true, ApprovalCanceled: true}, func(s ApprovalState) bool { return s.Terminal() })
}

func TestResultTransitions(t *testing.T) {
	testTransitions(t, allResultStates, map[ResultState][]ResultState{
		ResultCollecting:       {ResultSealed, ResultFailed, ResultUncertain, ResultRecoveryRequired},
		ResultUncertain:        {ResultCollecting, ResultSealed, ResultFailed, ResultRecoveryRequired},
		ResultRecoveryRequired: {ResultCollecting, ResultFailed},
	}, AllowResultTransition)
	testTerminal(t, allResultStates, map[ResultState]bool{ResultSealed: true, ResultFailed: true}, func(s ResultState) bool { return s.Terminal() })
}

func TestVerificationTransitions(t *testing.T) {
	testTransitions(t, allVerificationStates, map[VerificationState][]VerificationState{
		VerificationRequested:        {VerificationRunning, VerificationCanceled, VerificationRecoveryRequired},
		VerificationRunning:          {VerificationSucceeded, VerificationFailed, VerificationCancelRequested, VerificationUncertain, VerificationRecoveryRequired},
		VerificationCancelRequested:  {VerificationCanceled, VerificationFailed, VerificationUncertain, VerificationRecoveryRequired},
		VerificationUncertain:        {VerificationRunning, VerificationSucceeded, VerificationFailed, VerificationCanceled, VerificationRecoveryRequired},
		VerificationRecoveryRequired: {VerificationRequested, VerificationFailed, VerificationCanceled},
	}, AllowVerificationTransition)
	testTerminal(t, allVerificationStates, map[VerificationState]bool{VerificationSucceeded: true, VerificationFailed: true, VerificationCanceled: true}, func(s VerificationState) bool { return s.Terminal() })
}

func TestPublicationTransitions(t *testing.T) {
	testTransitions(t, allPublicationStates, map[PublicationState][]PublicationState{
		PublicationRequested:        {PublicationPreparing, PublicationCancelRequested, PublicationFailed, PublicationRecoveryRequired},
		PublicationPreparing:        {PublicationReady, PublicationFailed, PublicationConflict, PublicationUncertain, PublicationCancelRequested, PublicationRecoveryRequired},
		PublicationReady:            {PublicationPushing, PublicationCancelRequested, PublicationConflict, PublicationRecoveryRequired},
		PublicationPushing:          {PublicationOpeningPR, PublicationReconciling, PublicationFailed, PublicationConflict, PublicationUncertain, PublicationCancelRequested, PublicationRecoveryRequired},
		PublicationOpeningPR:        {PublicationReconciling, PublicationPublished, PublicationFailed, PublicationConflict, PublicationUncertain, PublicationCancelRequested, PublicationRecoveryRequired},
		PublicationReconciling:      {PublicationPushing, PublicationOpeningPR, PublicationPublished, PublicationFailed, PublicationConflict, PublicationCancelRequested, PublicationRecoveryRequired},
		PublicationCancelRequested:  {PublicationCanceled, PublicationReconciling, PublicationPublished, PublicationConflict, PublicationUncertain, PublicationRecoveryRequired},
		PublicationUncertain:        {PublicationReconciling, PublicationPublished, PublicationFailed, PublicationConflict, PublicationCanceled, PublicationRecoveryRequired},
		PublicationRecoveryRequired: {PublicationReconciling, PublicationFailed, PublicationConflict, PublicationCanceled},
	}, AllowPublicationTransition)
	testTerminal(t, allPublicationStates, map[PublicationState]bool{PublicationPublished: true, PublicationFailed: true, PublicationConflict: true, PublicationCanceled: true}, func(s PublicationState) bool { return s.Terminal() })
}

func testTransitions[T ~string](t *testing.T, states []T, want map[T][]T, allow func(T, T) error) {
	t.Helper()
	for _, from := range states {
		for _, to := range states {
			expected := false
			for _, candidate := range want[from] {
				expected = expected || candidate == to
			}
			err := allow(from, to)
			if expected && err != nil {
				t.Errorf("%s -> %s: unexpected error: %v", from, to, err)
			}
			if !expected && !errors.Is(err, ErrInvalidTransition) {
				t.Errorf("%s -> %s: got %v, want invalid transition", from, to, err)
			}
		}
	}
	var invalid T = "not_a_state"
	if !errors.Is(allow(invalid, states[0]), ErrInvalidState) || !errors.Is(allow(states[0], invalid), ErrInvalidState) {
		t.Error("unknown states must be invalid")
	}
}

func testTerminal[T comparable](t *testing.T, states []T, terminal map[T]bool, classify func(T) bool) {
	t.Helper()
	for _, state := range states {
		if classify(state) != terminal[state] {
			t.Errorf("terminal(%v) = %v, want %v", state, classify(state), terminal[state])
		}
	}
}
