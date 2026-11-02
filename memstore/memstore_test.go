package memstore_test

import (
	"testing"

	"github.com/nogo/faden"
	"github.com/nogo/faden/memstore"
	"github.com/nogo/faden/storetest"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) storetest.Storage {
		s := memstore.New()
		return storetest.Storage{Open: func() faden.Store { return s }}
	})
}

func TestNewStoreFindsNothingToContinue(t *testing.T) {
	s := memstore.New()
	if _, err := s.SubmitTurn(t.Context(), faden.Turn{RequestID: "r", Session: "s", Backend: "a"}); err != nil {
		t.Fatal(err)
	}
	left, err := memstore.New().Unfinished(t.Context())
	if err != nil || len(left) != 0 {
		t.Fatalf("unfinished = %+v, %v", left, err)
	}
}
