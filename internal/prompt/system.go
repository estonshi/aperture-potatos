package prompt

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Slot names, in the order the skeleton renders them.
const (
	SlotCore         = "core"
	SlotIdentity     = "identity"
	SlotCapabilities = "capabilities"
	SlotUser         = "user"
	SlotCatalog      = "catalog"
	SlotEnvironment  = "environment"
)

// Part is one rendered system section; the session file keeps only its text.
type Part struct {
	Slot string
	Name string
	Text string
}

type SystemInput struct {
	Nickname     string
	Description  string
	Instructions string
	Sections     []Part // extension sections, already sorted inside their slot
}

// System renders the fixed skeleton; the same input always yields the same bytes, which keeps prompt caches warm.
func System(in SystemInput) []Part {
	out := []Part{
		{Slot: SlotCore, Name: SlotCore, Text: File("system.core")},
		{Slot: SlotIdentity, Name: SlotIdentity, Text: Filef("identity", map[string]string{"nickname": in.Nickname, "description": in.Description})},
	}
	out = appendSlot(out, in.Sections, SlotCapabilities)
	if in.Instructions != "" {
		out = append(out, Part{Slot: SlotUser, Name: SlotUser, Text: Filef("user", map[string]string{"instructions": in.Instructions})})
	}
	out = appendSlot(out, in.Sections, SlotCatalog)
	return appendSlot(out, in.Sections, SlotEnvironment)
}

func appendSlot(out, sections []Part, slot string) []Part {
	for _, s := range sections {
		if s.Slot == slot && s.Text != "" {
			out = append(out, s)
		}
	}
	return out
}

// Hash is the system_hash of spec/runtime.md: hex SHA-256 of the section texts joined by NUL.
func Hash(texts []string) string {
	sum := sha256.Sum256([]byte(strings.Join(texts, "\x00")))
	return hex.EncodeToString(sum[:])
}
