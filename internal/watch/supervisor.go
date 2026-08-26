package watch

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"
)

type Supervisor struct {
	IdleAfter    time.Duration
	PauseTimeout time.Duration
	OnPause      func(context.Context) error
	Log          *slog.Logger
}

type statusProperties struct {
	SessionID string `json:"sessionID"`
	Status    struct {
		Type string `json:"type"`
	} `json:"status"`
}

type activityModel struct {
	epoch     uint64
	connected bool
	seenBusy  bool
	active    map[string]bool
}

type timerAction int

const (
	timerNone timerAction = iota
	timerCancel
	timerArm
)

func (s *Supervisor) Run(ctx context.Context, observations <-chan Observation) error {
	if s.IdleAfter <= 0 {
		return errors.New("idle duration must be positive")
	}
	if s.OnPause == nil {
		return errors.New("pause callback is required")
	}
	log := s.Log
	if log == nil {
		log = slog.Default()
	}
	pauseTimeout := s.PauseTimeout
	if pauseTimeout <= 0 {
		pauseTimeout = 30 * time.Second
	}

	timer := time.NewTimer(time.Hour)
	stopAndDrain(timer)
	defer timer.Stop()
	model := activityModel{active: make(map[string]bool)}
	armed := false
	var idleSince time.Time

	// handle applies one observation to the model, acts on the requested
	// timer change, and acknowledges delivery. Both the primary select loop
	// and the post-timer drain loop share it so neither can drift from the
	// other's apply->switch->acknowledge->timer-arm discipline.
	handle := func(observation Observation) {
		action := model.apply(observation)
		switch action {
		case timerCancel:
			if armed {
				stopAndDrain(timer)
				armed = false
			}
		case timerArm:
			stopAndDrain(timer)
			idleSince = time.Now()
			timer.Reset(s.IdleAfter)
			armed = true
			log.Info("workspace idle, arming pause", "epoch", model.epoch, "after", s.IdleAfter)
		}
		acknowledge(observation)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case observation, ok := <-observations:
			if !ok {
				return nil
			}
			handle(observation)
		case <-timer.C:
			armed = false
			// Prefer already-queued observations over a simultaneously-ready
			// timer. A queued disconnect or request invalidates this deadline.
		drain:
			for {
				select {
				case observation, ok := <-observations:
					if !ok {
						return nil
					}
					handle(observation)
				default:
					break drain
				}
			}
			if armed {
				continue
			}
			if !model.connected || !model.seenBusy || len(model.active) != 0 {
				continue
			}
			log.Info("pausing workspace", "epoch", model.epoch, "idle_for", time.Since(idleSince).Round(time.Second))
			pauseCtx, cancel := context.WithTimeout(ctx, pauseTimeout)
			err := s.OnPause(pauseCtx)
			cancel()
			if err != nil {
				log.Warn("pause deferred", "err", err)
				timer.Reset(min(s.IdleAfter, 5*time.Second))
				armed = true
				continue
			}
			model.seenBusy = false
			clear(model.active)
		}
	}
}

func acknowledge(observation Observation) {
	if observation.Handled != nil {
		close(observation.Handled)
	}
}

// apply is the policy core. The supervisor goroutine exclusively owns the
// model, so state changes are explicit and cannot escape through shared values.
func (model *activityModel) apply(observation Observation) timerAction {
	switch observation.Kind {
	case ObservationRequest:
		// A request after observed idle invalidates the old deadline, but retain
		// eligibility to retry through Manager's authoritative idle snapshots.
		// Otherwise a final browser request can prevent pause forever because no
		// subsequent session status transition is required from OpenCode.
		if model.connected && model.seenBusy && len(model.active) == 0 {
			return timerArm
		}
		model.seenBusy = false
		clear(model.active)
		return timerCancel
	case ObservationInvalidated:
		model.seenBusy = false
		clear(model.active)
		return timerCancel
	case ObservationConnected:
		if observation.Epoch < model.epoch {
			return timerNone
		}
		model.epoch = observation.Epoch
		model.connected = true
		model.seenBusy = false
		clear(model.active)
		return timerCancel
	case ObservationDisconnected:
		if observation.Epoch < model.epoch {
			return timerNone
		}
		model.epoch = observation.Epoch
		model.connected = false
		model.seenBusy = false
		clear(model.active)
		return timerCancel
	case ObservationStatus:
		if !model.connected || observation.Epoch != model.epoch {
			return timerNone
		}
		if observation.SessionID == "" {
			model.seenBusy = false
			clear(model.active)
			return timerCancel
		}
		switch observation.Status {
		case "busy", "retry":
			model.active[observation.SessionID] = true
			model.seenBusy = true
			return timerCancel
		case "idle":
			wasActive := model.active[observation.SessionID]
			delete(model.active, observation.SessionID)
			if wasActive && model.seenBusy && len(model.active) == 0 {
				return timerArm
			}
		default:
			model.seenBusy = false
			clear(model.active)
			return timerCancel
		}
	}
	return timerNone
}

func parseStatus(event Event) (sessionID, status string, ok bool) {
	payload := event.Properties
	if len(payload) == 0 {
		payload = event.Data
	}
	var properties statusProperties
	if err := json.Unmarshal(payload, &properties); err != nil {
		return "", "", false
	}
	if properties.SessionID == "" || properties.Status.Type == "" {
		return "", "", false
	}
	return properties.SessionID, properties.Status.Type, true
}

func stopAndDrain(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}
