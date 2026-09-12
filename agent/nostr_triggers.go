package agent

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

const (
	defaultNoticeLookback = 5 * time.Minute
	maxTriggerSeenIDs     = 4096
)

var safeTriggerTarget = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// NostrEventTriggerSource converts recent signed NostrHost notice events into
// a small fixed vocabulary of cycle triggers. Notice summaries are discarded.
type NostrEventTriggerSource struct {
	transport *RelayOperationTransport
	lookback  time.Duration
}

func NewNostrEventTriggerSource(transport *RelayOperationTransport, lookback time.Duration) (*NostrEventTriggerSource, error) {
	if transport == nil || transport.pool == nil {
		return nil, errors.New("Nostr event trigger source requires a relay transport")
	}
	if lookback < 0 || lookback > 24*time.Hour {
		return nil, errors.New("event trigger lookback must be between zero and 24 hours")
	}
	if lookback == 0 {
		lookback = defaultNoticeLookback
	}
	return &NostrEventTriggerSource{transport: transport, lookback: lookback}, nil
}

func (s *NostrEventTriggerSource) Start(ctx context.Context) <-chan CycleRequest {
	triggers := make(chan CycleRequest, 32)
	if s == nil || s.transport == nil || s.transport.pool == nil || ctx == nil {
		close(triggers)
		return triggers
	}
	since := nostr.Timestamp(time.Now().Add(-s.lookback).Unix())
	events := s.transport.pool.SubscribeMany(ctx, []string{s.transport.relayURL}, nostr.Filter{
		Kinds: []int{2210, 2211, 2212, 2213}, Authors: []string{s.transport.trustedServerKey},
		Since: &since, Limit: 256,
	})
	go s.forward(ctx, events, triggers)
	return triggers
}

func (s *NostrEventTriggerSource) forward(ctx context.Context, events <-chan nostr.RelayEvent, triggers chan<- CycleRequest) {
	defer close(triggers)
	seen := make(map[string]struct{})
	order := make([]string, 0, maxTriggerSeenIDs)
	for {
		select {
		case <-ctx.Done():
			return
		case received, open := <-events:
			if !open {
				return
			}
			if received.Event == nil {
				continue
			}
			request, ok := projectNoticeTrigger(received.Event, s.transport.trustedServerKey)
			if !ok {
				continue
			}
			if _, duplicate := seen[received.Event.ID]; duplicate {
				continue
			}
			seen[received.Event.ID] = struct{}{}
			order = append(order, received.Event.ID)
			if len(order) > maxTriggerSeenIDs {
				delete(seen, order[0])
				order = order[1:]
			}
			select {
			case triggers <- request:
			case <-ctx.Done():
				return
			}
		}
	}
}

func projectNoticeTrigger(event *nostr.Event, trustedServerKey string) (CycleRequest, bool) {
	if event == nil || !strings.EqualFold(event.PubKey, trustedServerKey) || !event.CheckID() {
		return CycleRequest{}, false
	}
	valid, err := event.CheckSignature()
	if err != nil || !valid {
		return CycleRequest{}, false
	}
	trigger := ""
	switch event.Kind {
	case 2210:
		trigger = "system_event"
	case 2211:
		trigger = "service_event"
	case 2212:
		trigger = "backup_event"
	case 2213:
		trigger = "security_event"
	default:
		return CycleRequest{}, false
	}
	var body struct {
		Severity string `json:"severity"`
		Target   string `json:"target"`
		App      string `json:"app"`
		Service  string `json:"service"`
	}
	if err := json.Unmarshal([]byte(event.Content), &body); err != nil || (body.Severity != "warning" && body.Severity != "critical") {
		return CycleRequest{}, false
	}
	targets := []string{body.Target, body.App, body.Service}
	target := ""
	for _, value := range targets {
		if value == "" {
			continue
		}
		if target != "" && target != value {
			return CycleRequest{}, false
		}
		target = value
	}
	if target != "" && !safeTriggerTarget.MatchString(target) {
		return CycleRequest{}, false
	}
	return CycleRequest{Trigger: trigger, Target: target}, true
}
