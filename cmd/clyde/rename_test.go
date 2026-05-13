package main

import (
	"bytes"
	"fmt"
	"testing"

	"goodkind.io/clyde/internal/session"
)

type renameStoreStub struct {
	sess    *session.Session
	resolve string
	updated *session.Session
}

func (s *renameStoreStub) Resolve(query string) (*session.Session, error) {
	s.resolve = query
	if s.sess == nil {
		return nil, nil
	}
	return s.sess, nil
}

func (s *renameStoreStub) Update(sess *session.Session) error {
	if sess == nil {
		return fmt.Errorf("nil session")
	}
	s.updated = sess
	return nil
}

func TestRunRenameTitleUpdatesMetadataTitleOnly(t *testing.T) {
	store := &renameStoreStub{
		sess: session.NewSession("stored-name", "session-123"),
	}
	var out bytes.Buffer

	if err := runRenameTitle(store, &out, "stored-name", "Clyde Title"); err != nil {
		t.Fatalf("runRenameTitle returned error: %v", err)
	}
	if store.resolve != "stored-name" {
		t.Fatalf("Resolve query = %q, want stored-name", store.resolve)
	}
	if store.updated == nil {
		t.Fatalf("Update was not called")
	}
	if store.updated.Name != "stored-name" {
		t.Fatalf("Name = %q, want stored-name", store.updated.Name)
	}
	if store.updated.Metadata.Title != "Clyde Title" {
		t.Fatalf("Title = %q, want Clyde Title", store.updated.Metadata.Title)
	}
	if got := out.String(); got != "Updated title for stored-name to \"Clyde Title\"\n" {
		t.Fatalf("output = %q", got)
	}
}
