package badger

import (
	suite "github.com/KinasTomes/miles/internal/metadata/contracttest"
	shared "github.com/KinasTomes/miles/internal/storage/contracttest"
	"testing"
)

func TestObservationContract(t *testing.T) {
	suite.RunObservation(t, func(t *testing.T, f shared.Fixture) suite.ObservingStore {
		m, _ := fixtureStore(t, f)
		t.Cleanup(func() { _ = m.Close() })
		return m
	})
}

func TestObservationCAS(t *testing.T) {
	suite.RunObservationCAS(t, func(t *testing.T, f shared.Fixture) suite.ObservingStore {
		m, _ := fixtureStore(t, f)
		t.Cleanup(func() { _ = m.Close() })
		return m
	})
}
