package app

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"nrtn.dev/catalyst/kpr/internal/policy"
	"nrtn.dev/catalyst/kpr/internal/store"
)

// notificationEnvelope is the distribution webhook body: a batch of
// registry events. Tag pushes record rows, tag deletes drop them;
// everything else is accepted and ignored.
type notificationEnvelope struct {
	Events []notificationEvent `json:"events"`
}

type notificationEvent struct {
	Action    string `json:"action"`
	Timestamp string `json:"timestamp"`
	Target    struct {
		Repository string `json:"repository"`
		Tag        string `json:"tag"`
		Digest     string `json:"digest"`
		MediaType  string `json:"mediaType"`
	} `json:"target"`
	// No actor: the notification's actor name carries no retention
	// meaning, so it is not even decoded.
}

// EventsHandler records distribution push notifications as tracked
// rows and drops rows on delete notifications: the tag is gone, so
// the plan must not reason about it anymore. A delete carries the
// registry's word (not a failed read), so no grace applies; a delete
// for an untracked tag is an idempotent no-op. When both the row
// and the event name a digest and they differ, the event is stale
// (replay or reorder past a re-push) and the row stays; a
// digest-less delete always drops, so replay safety ends where
// the digest is absent. The event timestamp
// is the push-time anchor eligibility counts from (a late
// notification must not grant extra life); an unparsable or missing
// timestamp falls back to now. A backend failure keeps the event
// accepted but logs the drop — visible, never silent.
func EventsHandler(s store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var env notificationEnvelope
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			http.Error(w, "bad notification envelope", http.StatusBadRequest)
			return
		}
		if s == nil {
			http.Error(w, "receiver not configured", http.StatusServiceUnavailable)
			return
		}
		for _, e := range env.Events {
			if e.Target.Repository == "" || e.Target.Tag == "" {
				continue
			}
			if e.Action == "delete" {
				if cur, ok, gerr := s.Get(r.Context(), e.Target.Repository, e.Target.Tag); gerr != nil {
					log.Printf("receiver: failed to read %s:%s for delete, stale row may linger: %v",
						e.Target.Repository, e.Target.Tag, gerr)
				} else if ok && cur.Digest != "" && e.Target.Digest != "" && cur.Digest != e.Target.Digest {
					log.Printf("receiver: stale delete for %s:%s (serves %s, event names %s): keeping the re-pushed row",
						e.Target.Repository, e.Target.Tag, cur.Digest, e.Target.Digest)
				} else if err := s.Delete(r.Context(), e.Target.Repository, e.Target.Tag); err != nil {
					log.Printf("receiver: failed to drop %s:%s, stale row lingers: %v",
						e.Target.Repository, e.Target.Tag, err)
				}
				continue
			}
			if e.Action != "push" {
				continue
			}
			pushedAt := time.Now().UTC()
			if ts, err := time.Parse(time.RFC3339, e.Timestamp); err == nil {
				pushedAt = ts.UTC()
			}
			// The actor is the recording component, never the pusher:
			// the notification's actor name (basic-auth username, or
			// nothing when anonymous) carries no retention meaning.
			row := policy.Row{
				Repo:      e.Target.Repository,
				Tag:       e.Target.Tag,
				Digest:    e.Target.Digest,
				MediaType: e.Target.MediaType,
				PushedAt:  pushedAt,
				Actor:     "kpr-receiver",
			}
			if err := s.Record(r.Context(), row); err != nil {
				log.Printf("receiver: failed to record %s:%s, push stays untracked: %v",
					row.Repo, row.Tag, err)
			}
		}
		w.WriteHeader(http.StatusAccepted)
	}
}
