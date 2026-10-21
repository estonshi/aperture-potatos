package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime/debug"

	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/store"
)

// safeStore turns a panic in the caller's store into an error with its stack, wherever the kernel calls it.
type safeStore struct{ in store.SessionStore }

// safeCallStore is a safeStore whose store also keeps the call ledger, so the kernel's CallStore check still sees it.
type safeCallStore struct {
	safeStore
	calls store.CallStore
}

func guardStore(st store.SessionStore) store.SessionStore {
	switch v := st.(type) {
	case safeStore, safeCallStore:
		return st
	case store.CallStore:
		return safeCallStore{safeStore{st}, v}
	}
	return safeStore{st}
}

// safeLabels guards a store's LabelStore apart from its SessionStore, so the optional interfaces need no combined wrapper.
type safeLabels struct{ in store.LabelStore }

func (s safeLabels) SaveLabels(ctx context.Context, rec store.LabelRecord) (err error) {
	defer storePanic("SaveLabels", &err)
	return s.in.SaveLabels(ctx, rec)
}

func (s safeLabels) LoadLabels(ctx context.Context, id string) (_ store.LabelRecord, _ bool, err error) {
	defer storePanic("LoadLabels", &err)
	return s.in.LoadLabels(ctx, id)
}

func (s safeLabels) FindLabels(ctx context.Context, match map[string]string) (_ []store.LabelRecord, err error) {
	defer storePanic("FindLabels", &err)
	return s.in.FindLabels(ctx, match)
}

func storePanic(op string, err *error) {
	if r := recover(); r != nil {
		*err = fmt.Errorf("store %s panicked: %v\n%s", op, r, debug.Stack())
	}
}

func (s safeStore) Append(ctx context.Context, id string, msgs []message.Message) (err error) {
	defer storePanic("Append", &err)
	return s.in.Append(ctx, id, msgs)
}

func (s safeStore) Load(ctx context.Context, id string) (_ []message.Message, err error) {
	defer storePanic("Load", &err)
	return s.in.Load(ctx, id)
}

func (s safeStore) List(ctx context.Context, prefix string) (_ []string, err error) {
	defer storePanic("List", &err)
	return s.in.List(ctx, prefix)
}

func (s safeCallStore) AppendCalls(ctx context.Context, id string, calls []json.RawMessage) (err error) {
	defer storePanic("AppendCalls", &err)
	return s.calls.AppendCalls(ctx, id, calls)
}

func (s safeCallStore) LoadCalls(ctx context.Context, id string) (_ []json.RawMessage, err error) {
	defer storePanic("LoadCalls", &err)
	return s.calls.LoadCalls(ctx, id)
}
