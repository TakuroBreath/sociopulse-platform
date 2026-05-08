// topics_test.go — exhaustive coverage for the AllTopics slice and the
// TopicAction enum.
//
// Why exhaustive:
//
//   - The Hub's RBAC pre-flight at the subscribe boundary uses AllTopics
//     to validate inbound topic strings. A topic constant added without
//     an AllTopics entry would be silently rejected as "unknown" — this
//     test catches that drift.
//   - TopicAction labels feed an audit_log field that is queried by
//     supervisors looking up listen-in / force-command history. An
//     undeclared action label silently disappears from those queries.
package api_test

import (
	"slices"
	"testing"

	"github.com/sociopulse/platform/internal/realtime/api"
)

func TestAllTopics_ContainsEveryDeclaredConstant(t *testing.T) {
	t.Parallel()

	want := []api.Topic{
		api.TopicOperatorsState,
		api.TopicDialerQueue,
		api.TopicTrunksHealth,
		api.TopicCallEvents,
		api.TopicNotifications,
		api.TopicForceCommands,
	}
	got := api.AllTopics
	if len(got) != len(want) {
		t.Fatalf("AllTopics has %d entries, want %d", len(got), len(want))
	}
	for _, topic := range want {
		if !slices.Contains(got, topic) {
			t.Errorf("AllTopics missing %q — adding a topic constant requires extending AllTopics", topic)
		}
	}
}

func TestAllTopics_NoUnknownEntries(t *testing.T) {
	t.Parallel()

	known := map[api.Topic]struct{}{
		api.TopicOperatorsState: {},
		api.TopicDialerQueue:    {},
		api.TopicTrunksHealth:   {},
		api.TopicCallEvents:     {},
		api.TopicNotifications:  {},
		api.TopicForceCommands:  {},
	}
	for _, topic := range api.AllTopics {
		if _, ok := known[topic]; !ok {
			t.Errorf("AllTopics contains unknown entry %q", topic)
		}
	}
}

func TestTopicAction_KnownValues(t *testing.T) {
	t.Parallel()

	cases := []api.TopicAction{
		api.ActionSubscribe,
		api.ActionPublish,
	}
	for _, a := range cases {
		t.Run(string(a), func(t *testing.T) {
			t.Parallel()
			if !a.Valid() {
				t.Fatalf("%q: want Valid() true, got false", a)
			}
		})
	}
}

func TestTopicAction_RejectsUnknown(t *testing.T) {
	t.Parallel()

	cases := []string{
		"",            // zero value
		"SUBSCRIBE",   // wrong case
		"subscribe ",  // trailing space
		"unsubscribe", // close-but-wrong
		"unknown",     // garbage
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			if api.TopicAction(raw).Valid() {
				t.Fatalf("%q: want Valid() false, got true", raw)
			}
		})
	}
}
