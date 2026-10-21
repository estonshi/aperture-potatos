package kernel

import "github.com/estonshi/aperture-potatos/event"

// Emit publishes an extension event on the session outside any call, so a command, context provider or hook can report what it did.
func (s *Session) Emit(p event.Payload) { s.publish(p) }

// Announce tells the model something with its next turn, as a reminder with source that does not wake; p, when not nil, is published just before it is queued.
func (s *Session) Announce(source, text string, p event.Payload) {
	s.post(func(st *state) {
		if st.phase == phaseClosing || st.phase == phaseClosed {
			return
		}
		if p != nil {
			s.publish(p)
		}
		if text != "" {
			s.enqueue(s.reminderItem(source, text, false))
		}
	})
}

// Announce reaches every open session of this agent; like a registry change, it does not reach the sessions of agents derived from it.
func (a *Agent) Announce(source, text string, p event.Payload) {
	for _, s := range a.sessions() {
		s.Announce(source, text, p)
	}
}
