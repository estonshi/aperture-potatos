package kernel

import (
	"context"
	"fmt"
	"maps"

	"github.com/estonshi/aperture-potatos/store"
)

// SessionLabels is one stored session a label query found.
type SessionLabels struct {
	SessionID string
	Labels    map[string]string
}

func (s *Session) labelMap() map[string]string {
	if p := s.labels.Load(); p != nil {
		return *p
	}
	return nil
}

// SetLabels merges changes into the session's labels: a key with an empty value is removed, other keys stay; the record is stored before it returns.
func (s *Session) SetLabels(ctx context.Context, changes map[string]string) error {
	ls := s.agent.cfg.labels
	if ls == nil {
		return ErrNoLabelStore
	}
	if _, ok := changes[""]; ok {
		return fmt.Errorf("%w: empty label key", ErrInvalidName)
	}
	select {
	case <-s.done:
		return ErrSessionClosed
	default:
	}
	if len(changes) == 0 {
		return nil
	}
	s.labelsMu.Lock()
	defer s.labelsMu.Unlock()
	next := mergeLabels(s.labelMap(), changes)
	if err := ls.SaveLabels(ctx, store.LabelRecord{SessionID: s.id, ParentSessionID: s.parentID(), Labels: next}); err != nil {
		return err
	}
	s.labels.Store(&next)
	return nil
}

func mergeLabels(base, changes map[string]string) map[string]string {
	next := maps.Clone(base)
	if next == nil {
		next = map[string]string{}
	}
	for k, v := range changes {
		if v == "" {
			delete(next, k)
		} else {
			next[k] = v
		}
	}
	return next
}

// FindSessions lists the stored top-level sessions whose labels hold every given pair, in session id order; side chats and task child sessions are left out.
func (a *Agent) FindSessions(ctx context.Context, labels map[string]string) ([]SessionLabels, error) {
	ls := a.cfg.labels
	if ls == nil {
		return nil, ErrNoLabelStore
	}
	recs, err := ls.FindLabels(ctx, labels)
	if err != nil {
		return nil, err
	}
	var out []SessionLabels
	for _, r := range recs {
		if r.ParentSessionID == "" {
			out = append(out, SessionLabels{SessionID: r.SessionID, Labels: r.Labels})
		}
	}
	return out, nil
}

// saveLabels stores the session's label record whole, as its first epoch is written or a resume settles its labels.
func (s *Session) saveLabels(ctx context.Context) error {
	if ls := s.agent.cfg.labels; ls != nil {
		return ls.SaveLabels(ctx, store.LabelRecord{SessionID: s.id, ParentSessionID: s.parentID(), Labels: s.labelMap()})
	}
	return nil
}

// resumedLabels are the labels a resume opens with: the stored record over the file's first line, with WithLabels merged in; save says the record must be written.
func (a *Agent) resumedLabels(ctx context.Context, id string, created, given map[string]string) (labels map[string]string, save bool, err error) {
	ls := a.cfg.labels
	if ls == nil {
		if given != nil {
			return given, false, nil // without a record, WithLabels holds for this opening only
		}
		return created, false, nil
	}
	rec, ok, err := ls.LoadLabels(ctx, id)
	if err != nil {
		return nil, false, err
	}
	labels = created
	if ok {
		labels = rec.Labels
	}
	if given != nil {
		return mergeLabels(labels, given), true, nil
	}
	return labels, !ok, nil // a session from before label records gets one now
}
